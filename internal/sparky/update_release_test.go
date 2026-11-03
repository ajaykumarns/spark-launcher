package sparky

// Focused tests for the verified-release library in update_release.go and
// update_release_archive.go. Everything is offline: a fake RoundTripper serves
// synthetic GitHub API, tag-ref, metadata, checksum and asset responses over an
// in-memory router. No real network, host, TLS or execution is involved. Each
// test breaks exactly one invariant to prove the matching path fails closed.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// updateTestCommit is a fixed full commit used across fixtures.
const updateTestCommit = "0123456789abcdef0123456789abcdef01234567"

// updateFixture is one fully consistent release. Mutate a low-level source
// (f.arch bytes, per-binary digests) then call f.finalize(); the resolver and
// every served document stay mutually consistent, so a targeted edit is the
// only thing that can fail.
type updateFixture struct {
	t       *testing.T
	version string
	tag     string
	arch    map[string][]byte            // platform -> gzip archive bytes
	binSHA  map[string]map[string]string // platform -> binary -> digest
	sums    map[string]string            // asset name -> digest (SHA256SUMS)
	apiDg   map[string]string            // asset name -> API digest
	meta    map[string]any               // release-metadata.json
	commit  string
	served  map[string][]byte // rendered document/asset bodies
	draft   bool
	rateLim bool
	rateSet string
	redir   string // object-store host to redirect asset requests to, or ""
}

func updateNewFixture(t *testing.T, version string) *updateFixture {
	t.Helper()
	f := &updateFixture{
		t: t, version: version, tag: "v" + version, commit: updateTestCommit,
		arch: map[string][]byte{}, binSHA: map[string]map[string]string{},
		sums: map[string]string{}, apiDg: map[string]string{}, meta: map[string]any{},
		served: map[string][]byte{},
	}
	for _, platform := range []string{"darwin/arm64", "linux/arm64"} {
		f.buildPlatform(platform, true)
	}
	f.finalize()
	return f
}

// buildPlatform (re)creates one platform's archive with ARM64 (or wrong-arch)
// binaries and matching BUILDINFO/binary digests.
func (f *updateFixture) buildPlatform(platform string, arm64 bool) {
	osName, _ := updatePlatformOS(platform)
	// Keep any previously injected extra members (added via addMember).
	cli := updateSyntheticExecutable(f.t, osName, arm64)
	inst := updateSyntheticExecutable(f.t, osName, arm64)
	cliSHA := hex.EncodeToString(hashBytes(cli))
	instSHA := hex.EncodeToString(hashBytes(inst))
	f.binSHA[platform] = map[string]string{"sparky": cliSHA, "sparky-install": instSHA}
	top := "sparky-" + f.version + "-" + osName + "-arm64"
	buildInfo, _ := json.Marshal(map[string]any{
		"name": "sparky", "version": f.version, "tag": f.tag, "commit": f.commit, "platform": platform,
		"binaries": []any{
			map[string]any{"path": "bin/sparky", "sha256": cliSHA},
			map[string]any{"path": "bin/sparky-install", "sha256": instSHA},
		},
	})
	files := map[string]tarEntry{
		top + "/bin/sparky":         {mode: 0o755, data: cli},
		top + "/bin/sparky-install": {mode: 0o755, data: inst},
		top + "/BUILDINFO.json":     {mode: 0o644, data: buildInfo},
	}
	f.arch[platform] = updateTarGz(f.t, files)
}

// finalize re-derives every served digest from the low-level sources so the
// checksums-file, archive, API and binary digests are internally consistent.
func (f *updateFixture) finalize() {
	assets := []any{}
	f.sums = map[string]string{}
	f.apiDg = map[string]string{}
	for _, platform := range []string{"darwin/arm64", "linux/arm64"} {
		osName, _ := updatePlatformOS(platform)
		name := "sparky-" + f.version + "-" + osName + "-arm64.tar.gz"
		sum := hex.EncodeToString(hashBytes(f.arch[platform]))
		f.sums[name] = sum
		f.apiDg[name] = sum
		assets = append(assets, map[string]any{
			"file": name, "platform": platform, "kind": "cli-and-installer",
			"sha256": sum, "size_bytes": len(f.arch[platform]),
		})
	}
	sumsBody := f.sumsBody()
	f.meta = map[string]any{
		"name": "sparky", "tag": f.tag, "version": f.version, "commit": f.commit,
		"platforms":        []string{"darwin/arm64", "linux/arm64"},
		"checksums_file":   "SHA256SUMS",
		"checksums_sha256": hex.EncodeToString(hashBytes([]byte(sumsBody))),
		"assets":           assets,
		"binary_sha256":    f.binSHA,
	}
}

func (f *updateFixture) sumsBody() string {
	names := make([]string, 0, len(f.sums))
	for n := range f.sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", f.sums[n], n)
	}
	return b.String()
}

// setMetaAsset overrides one metadata asset's recorded sha (post-finalize).
func (f *updateFixture) setMetaAsset(file, sha string) {
	for _, a := range f.meta["assets"].([]any) {
		m := a.(map[string]any)
		if m["file"] == file {
			m["sha256"] = sha
		}
	}
}

// setMetaBinary overrides one metadata binary_sha256 entry (post-finalize).
func (f *updateFixture) setMetaBinary(platform, name, sha string) {
	f.meta["binary_sha256"].(map[string]map[string]string)[platform][name] = sha
}

// --- synthetic executables --------------------------------------------------

func updateSyntheticExecutable(t *testing.T, osName string, arm64 bool) []byte {
	t.Helper()
	if osName == "linux" {
		return updateSyntheticELF(arm64)
	}
	return updateSyntheticMachO(arm64)
}

// updateSyntheticELF builds a tiny valid ET_EXEC, ET_DYN-independent 64-bit
// little-endian ELF header with no section/program headers.
func updateSyntheticELF(arm64 bool) []byte {
	hdr := make([]byte, 64)
	copy(hdr[0:4], []byte{0x7f, 'E', 'L', 'F'})
	hdr[4] = 2                                   // ELFCLASS64
	hdr[5] = 1                                   // ELFDATA2LSB
	hdr[6] = 1                                   // EV_CURRENT
	binary.LittleEndian.PutUint16(hdr[16:18], 2) // e_type = ET_EXEC
	mach := uint16(62)                           // EM_X86_64
	if arm64 {
		mach = 183 // EM_AARCH64
	}
	binary.LittleEndian.PutUint16(hdr[18:20], mach)
	binary.LittleEndian.PutUint32(hdr[20:24], 1)  // e_version = EV_CURRENT
	binary.LittleEndian.PutUint64(hdr[32:40], 64) // e_phoff
	binary.LittleEndian.PutUint64(hdr[40:48], 0)  // e_shoff
	binary.LittleEndian.PutUint16(hdr[52:54], 64) // e_ehsize
	binary.LittleEndian.PutUint16(hdr[54:56], 56) // e_phentsize
	binary.LittleEndian.PutUint16(hdr[56:58], 0)  // e_phnum
	binary.LittleEndian.PutUint16(hdr[58:60], 64) // e_shentsize
	binary.LittleEndian.PutUint16(hdr[60:62], 0)  // e_shnum
	return hdr
}

// updateSyntheticMachO builds a tiny valid 64-bit MH_EXECUTE Mach-O header.
func updateSyntheticMachO(arm64 bool) []byte {
	buf := &bytes.Buffer{}
	binary.Write(buf, binary.LittleEndian, uint32(0xfeedfacf)) // MH_MAGIC_64
	if arm64 {
		binary.Write(buf, binary.LittleEndian, uint32(0x0100000c)) // CPU_TYPE_ARM64
		binary.Write(buf, binary.LittleEndian, uint32(0))
	} else {
		binary.Write(buf, binary.LittleEndian, uint32(0x01000007)) // CPU_TYPE_X86_64
		binary.Write(buf, binary.LittleEndian, uint32(3))
	}
	binary.Write(buf, binary.LittleEndian, uint32(2)) // MH_EXECUTE
	binary.Write(buf, binary.LittleEndian, uint32(0)) // ncmds
	binary.Write(buf, binary.LittleEndian, uint32(0)) // sizeofcmds
	binary.Write(buf, binary.LittleEndian, uint32(0)) // flags
	return buf.Bytes()
}

// --- archive helpers --------------------------------------------------------

type tarEntry struct {
	mode int64
	data []byte
}

func updateTarGz(t *testing.T, files map[string]tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		e := files[name]
		tw.WriteHeader(&tar.Header{Name: name, Mode: e.mode, Size: int64(len(e.data)), Typeflag: tar.TypeReg})
		tw.Write(e.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func hashBytes(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func mustOS(platform string) string { o, _ := updatePlatformOS(platform); return o }

// addMember appends an arbitrary tar member to a platform archive then
// re-derives all digests so only the extractor can reject the new member.
func (f *updateFixture) addMember(platform, name string, hdr *tar.Header, data []byte) {
	osName := mustOS(platform)
	members := readTarGz(f.t, f.arch[platform])
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		tw.WriteHeader(m.hdr)
		tw.Write(m.data)
	}
	tw.WriteHeader(hdr)
	tw.Write(data)
	tw.Close()
	gz.Close()
	f.arch[platform] = buf.Bytes()
	_ = osName
	f.finalize()
}

type tarMember struct {
	hdr  *tar.Header
	data []byte
}

func readTarGz(t *testing.T, archive []byte) []tarMember {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out []tarMember
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, _ := io.ReadAll(tr)
		out = append(out, tarMember{hdr: h, data: data})
	}
	return out
}

// --- fake transport ---------------------------------------------------------

func (f *updateFixture) serve(w http.ResponseWriter, r *http.Request) {
	if f.rateLim {
		w.Header().Set("X-RateLimit-Remaining", "0")
		if f.rateSet != "" {
			w.Header().Set("X-RateLimit-Reset", f.rateSet)
		}
		w.WriteHeader(http.StatusForbidden)
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/releases/latest"), strings.HasSuffix(p, "/releases/tags/"+f.tag):
		assets := []any{}
		for name, d := range f.apiDg {
			assets = append(assets, map[string]any{"name": name, "digest": "sha256:" + d})
		}
		assets = append(assets, map[string]any{"name": "SHA256SUMS"}, map[string]any{"name": "release-metadata.json"})
		body, _ := json.Marshal(map[string]any{"tag_name": f.tag, "draft": f.draft, "prerelease": false, "assets": assets})
		w.Write(body)
	case strings.Contains(p, "/git/ref/tags/"):
		body, _ := json.Marshal(map[string]any{"object": map[string]any{"sha": f.commit, "type": "commit"}})
		w.Write(body)
	case strings.HasSuffix(p, "/release-metadata.json"):
		body, err := json.Marshal(f.meta)
		if err != nil {
			f.t.Fatalf("marshal meta: %v", err)
		}
		w.Write(body)
	case strings.HasSuffix(p, "/SHA256SUMS"):
		w.Write([]byte(f.sumsBody()))
	default:
		for _, platform := range []string{"darwin/arm64", "linux/arm64"} {
			name := "sparky-" + f.version + "-" + mustOS(platform) + "-arm64.tar.gz"
			if strings.HasSuffix(p, "/"+name) {
				if f.redir != "" && r.URL.Host == "github.com" {
					http.Redirect(w, r, "https://"+f.redir+"/"+name, http.StatusFound)
					return
				}
				w.Write(f.arch[platform])
				return
			}
		}
		http.NotFound(w, r)
	}
}

func withFakeTransport(t *testing.T, f *updateFixture) {
	t.Helper()
	withFakeHandler(t, http.HandlerFunc(f.serve))
}

func withFakeHandler(t *testing.T, h http.Handler) {
	t.Helper()
	prev := updateReleaseTransport
	updateReleaseTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec := &fakeResponseWriter{header: http.Header{}, code: 200, body: &bytes.Buffer{}}
		h.ServeHTTP(rec, req)
		return &http.Response{
			StatusCode:    rec.code,
			Header:        rec.header,
			Body:          io.NopCloser(bytes.NewReader(rec.body.Bytes())),
			ContentLength: int64(rec.body.Len()),
		}, nil
	})
	t.Cleanup(func() { updateReleaseTransport = prev })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (rt roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

type fakeResponseWriter struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func (w *fakeResponseWriter) Header() http.Header         { return w.header }
func (w *fakeResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *fakeResponseWriter) WriteHeader(code int)        { w.code = code }

// --- resolve tests ----------------------------------------------------------

func TestUpdateResolveLatestSuccess(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeTransport(t, f)
	rel, err := updateResolveRelease(context.Background(), "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rel.Tag != "v0.6.4" || rel.Version != "0.6.4" || rel.Commit != updateTestCommit {
		t.Fatalf("unexpected release: %+v", rel)
	}
	for _, platform := range []string{"darwin/arm64", "linux/arm64"} {
		a := rel.Assets[platform]
		want := "https://github.com/ajaykumarns/spark-launcher/releases/download/v0.6.4/sparky-0.6.4-" + mustOS(platform) + "-arm64.tar.gz"
		if a.URL != want {
			t.Fatalf("asset url %q != %q", a.URL, want)
		}
		if !isHexDigest(a.SHA256) || a.Size <= 0 {
			t.Fatalf("asset fields bad: %+v", a)
		}
		if len(rel.BinarySHA256[platform]) != 2 {
			t.Fatalf("binary hashes missing for %s", platform)
		}
	}
}

func TestUpdateResolveTaggedSuccess(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeTransport(t, f)
	rel, err := updateResolveRelease(context.Background(), "v0.6.4")
	if err != nil || rel.Tag != "v0.6.4" {
		t.Fatalf("tagged resolve: %v", err)
	}
}

func TestUpdateResolveRejectsDraft(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.draft = true
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "prerelease") {
		t.Fatalf("expected draft rejection, got %v", err)
	}
}

func TestUpdateResolveRateLimit(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.rateLim = true
	f.rateSet = "1700000000"
	withFakeTransport(t, f)
	_, err := updateResolveRelease(context.Background(), "")
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	msg := err.Error()
	if !(strings.Contains(msg, "rate limit") || strings.Contains(msg, "resets")) {
		t.Fatalf("missing rate-limit guidance: %s", msg)
	}
	if strings.Contains(strings.ToLower(msg), "bypass") {
		t.Fatalf("must not imply a bypass: %s", msg)
	}
}

func TestUpdateResolveRateLimitTagDoesNotBypass(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.rateLim = true
	withFakeTransport(t, f)
	_, err := updateResolveRelease(context.Background(), "v0.6.4")
	if err == nil || !strings.Contains(err.Error(), "does not lift") {
		t.Fatalf("expected tag-does-not-bypass guidance, got %v", err)
	}
}

func TestUpdateResolveMalformedAPI(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.served["api"] = []byte("{not json")
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Write([]byte("{not json"))
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err == nil {
		t.Fatal("expected malformed API error")
	}
}

func TestUpdateResolveMetadataCommitInvalid(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["commit"] = "not-a-commit"
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("expected invalid commit rejection, got %v", err)
	}
}

func TestUpdateResolveTagCommitMismatch(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/ref/tags/") {
			w.Write([]byte(`{"object":{"sha":"ffffffffffffffffffffffffffffffffffffffff","type":"commit"}}`))
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "tag points at commit") {
		t.Fatalf("expected tag commit mismatch, got %v", err)
	}
}

func TestUpdateResolveChecksumsFileMismatch(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["checksums_sha256"] = strings.Repeat("0", 64)
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("expected checksums-file mismatch, got %v", err)
	}
}

func TestUpdateResolveArchiveDigestMismatch(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	// Metadata claims a wrong archive digest while SHA256SUMS and the API stay
	// truthful, so the "SHA256SUMS does not confirm" branch fires.
	f.setMetaAsset("sparky-0.6.4-linux-arm64.tar.gz", strings.Repeat("1", 64))
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "does not confirm") {
		t.Fatalf("expected archive digest mismatch, got %v", err)
	}
}

func TestUpdateResolveAPIDigestDisagrees(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.apiDg["sparky-0.6.4-darwin-arm64.tar.gz"] = strings.Repeat("2", 64)
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "asset digest") {
		t.Fatalf("expected API digest disagreement, got %v", err)
	}
}

func TestUpdateResolveBinarySHA256Invalid(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.setMetaBinary("linux/arm64", "sparky", "nothex")
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "invalid binary digest") {
		t.Fatalf("expected invalid binary digest, got %v", err)
	}
}

func TestUpdateResolvePredatesUpdater(t *testing.T) {
	f := updateNewFixture(t, "0.6.3")
	delete(f.meta, "binary_sha256")
	withFakeTransport(t, f)
	_, err := updateResolveRelease(context.Background(), "v0.6.3")
	if err == nil || !strings.Contains(err.Error(), "predates the sparky updater") {
		t.Fatalf("expected predates-updater error, got %v", err)
	}
}

// --- metadata contract fields -----------------------------------------------

func TestUpdateResolvePlatformsMissing(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	delete(f.meta, "platforms")
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "does not list the supported platform") {
		t.Fatalf("expected missing-platforms rejection, got %v", err)
	}
}

func TestUpdateResolvePlatformsEmpty(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["platforms"] = []string{}
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "does not list the supported platform darwin/arm64") {
		t.Fatalf("expected empty-platforms rejection, got %v", err)
	}
}

func TestUpdateResolvePlatformsIncomplete(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["platforms"] = []string{"darwin/arm64"}
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "does not list the supported platform linux/arm64") {
		t.Fatalf("expected incomplete-platforms rejection, got %v", err)
	}
}

func TestUpdateResolvePlatformsUnsupported(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["platforms"] = []string{"darwin/arm64", "linux/amd64"}
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("expected unsupported-platform rejection, got %v", err)
	}
}

func TestUpdateResolvePlatformsDuplicate(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["platforms"] = []string{"darwin/arm64", "darwin/arm64", "linux/arm64"}
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("expected duplicate-platform rejection, got %v", err)
	}
}

func TestUpdateResolvePlatformsWrongType(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/release-metadata.json") {
			fmt.Fprintf(w, `{"tag":%q,"version":%q,"commit":%q,"platforms":"darwin/arm64 linux/arm64"}`,
				f.tag, f.version, f.commit)
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed platforms rejection, got %v", err)
	}
}

func TestUpdateResolveChecksumsFileMissing(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	delete(f.meta, "checksums_file")
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "checksums file") || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("expected missing checksums_file rejection, got %v", err)
	}
}

func TestUpdateResolveChecksumsFileWrongValue(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.meta["checksums_file"] = "sha256sums.txt"
	withFakeTransport(t, f)
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "checksums file") {
		t.Fatalf("expected wrong checksums_file rejection, got %v", err)
	}
}

// --- GitHub API decoding ----------------------------------------------------

func TestUpdateResolveAPIToleratesUnknownFields(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	assets := []any{}
	for _, platform := range []string{"darwin/arm64", "linux/arm64"} {
		name := "sparky-0.6.4-" + mustOS(platform) + "-arm64.tar.gz"
		assets = append(assets, map[string]any{
			"name": name, "digest": "sha256:" + f.apiDg[name], "size": 1,
			"node_id": "RA_kwDO", "content_type": "application/gzip", "label": nil,
			"uploader": map[string]any{"login": "publisher"},
		})
	}
	body, err := json.Marshal(map[string]any{
		"tag_name": f.tag, "name": "Sparky 0.6.4", "draft": false, "prerelease": false,
		"target_commitish": "main", "published_at": "2026-01-01T00:00:00Z",
		"assets": assets, "future_field": map[string]any{"flags": []any{1, 2, 3}},
	})
	if err != nil {
		t.Fatalf("marshal api doc: %v", err)
	}
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Write(body)
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err != nil {
		t.Fatalf("unknown API fields must not fail a valid release: %v", err)
	}
}

func TestUpdateResolveAPIRequiredFieldStillChecked(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Write([]byte(`{"name":"Sparky","draft":false,"prerelease":false,"assets":[]}`))
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "no tag") {
		t.Fatalf("expected required-field rejection, got %v", err)
	}
}

func TestUpdateResolveAPITrailingDataRejected(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Write([]byte(`{"tag_name":"v0.6.4","draft":false,"prerelease":false,"assets":[]}{"x":1}`))
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("expected trailing-data rejection, got %v", err)
	}
}

func TestUpdateResolveAPISizeCapEnforced(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	pad := strings.Repeat("a", updateMaxReleaseJSON+updateMaxBodySlack+1)
	body := []byte(`{"tag_name":"v0.6.4","draft":false,"prerelease":false,"assets":[],"pad":"` + pad + `"}`)
	withFakeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Write(body)
			return
		}
		f.serve(w, r)
	}))
	if _, err := updateResolveRelease(context.Background(), ""); err == nil {
		t.Fatal("expected an oversized API document to be refused")
	}
}

// --- prepare tests ----------------------------------------------------------

func TestUpdatePrepareDarwinSuccess(t *testing.T) { updatePrepareSuccess(t, "darwin/arm64") }
func TestUpdatePrepareLinuxSuccess(t *testing.T)  { updatePrepareSuccess(t, "linux/arm64") }

func updatePrepareSuccess(t *testing.T, platform string) {
	t.Helper()
	f := updateNewFixture(t, "0.6.4")
	withFakeTransport(t, f)
	rel, err := updateResolveRelease(context.Background(), "v0.6.4")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	dest := t.TempDir()
	prep, err := updatePrepareAsset(context.Background(), rel, platform, dest)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if filepath.Base(prep.Dir) != "sparky-0.6.4-"+mustOS(platform)+"-arm64" {
		t.Fatalf("unexpected dir %q", prep.Dir)
	}
	if prep.CLI != filepath.Join(prep.Dir, "bin", "sparky") || prep.Installer != filepath.Join(prep.Dir, "bin", "sparky-install") {
		t.Fatalf("binary paths wrong: %+v", prep)
	}
	if prep.CLISHA != f.binSHA[platform]["sparky"] || prep.InstallerSHA != f.binSHA[platform]["sparky-install"] {
		t.Fatalf("digests wrong: %+v", prep)
	}
	for _, p := range []string{prep.CLI, prep.Installer} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm()&0o100 == 0 {
			t.Fatalf("binary not executable: %v %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(prep.Dir, "BUILDINFO.json")); err != nil {
		t.Fatalf("BUILDINFO missing: %v", err)
	}
	st, _ := os.Stat(filepath.Dir(prep.Dir)) // the private staging root
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("staging root mode %v != 0700", st.Mode().Perm())
	}
}

func TestUpdatePrepareCorruptArchive(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	// Flip a byte in the served archive WITHOUT re-finalizing, so the digest the
	// library expects no longer matches the bytes it receives.
	flipped := append([]byte(nil), f.arch["linux/arm64"]...)
	flipped[len(flipped)-1] ^= 0xff
	f.arch["linux/arm64"] = flipped // served bytes; metadata still has the old digest
	withFakeTransport(t, f)
	rel, err := updateResolveRelease(context.Background(), "v0.6.4")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expected corrupt-archive digest rejection, got %v", err)
	}
}

func TestUpdatePrepareManifestDisagreement(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	// BUILDINFO (in the archive) lists the true digest; release-metadata lies.
	f.setMetaBinary("linux/arm64", "sparky", strings.Repeat("4", 64))
	withFakeTransport(t, f)
	rel, err := updateResolveRelease(context.Background(), "v0.6.4")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "manifests disagree") {
		t.Fatalf("expected manifest disagreement, got %v", err)
	}
}

func TestUpdatePrepareWrongArch(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	// Rebuild linux with x86-64 binaries, fully consistent, so only the arch
	// check can fail.
	f.buildPlatform("linux/arm64", false)
	f.finalize()
	withFakeTransport(t, f)
	rel, err := updateResolveRelease(context.Background(), "v0.6.4")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "ARM64") {
		t.Fatalf("expected wrong-arch rejection, got %v", err)
	}
}

func TestUpdatePrepareTraversal(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.addMember("linux/arm64", "../evil", &tar.Header{Name: "../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}, []byte("x"))
	dest := t.TempDir()
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", dest); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if _, err := os.Stat(filepath.Join(dest, "evil")); !os.IsNotExist(err) {
		t.Fatal("traversal file escaped staging")
	}
}

func TestUpdatePrepareSymlink(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.addMember("linux/arm64", "sparky-0.6.4-linux-arm64/evil", &tar.Header{
		Name: "sparky-0.6.4-linux-arm64/evil", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
	}, nil)
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestUpdatePrepareUnexpectedFile(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.addMember("linux/arm64", "sparky-0.6.4-linux-arm64/bin/surprise", &tar.Header{
		Name: "sparky-0.6.4-linux-arm64/bin/surprise", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
	}, []byte("x"))
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("expected unexpected-file rejection, got %v", err)
	}
}

func TestUpdatePrepareMaxFile(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	size := deploymentMaxPayload + 1
	f.addMember("linux/arm64", "sparky-0.6.4-linux-arm64/LICENSE", &tar.Header{
		Name: "sparky-0.6.4-linux-arm64/LICENSE", Mode: 0o644, Size: int64(size), Typeflag: tar.TypeReg,
	}, make([]byte, size))
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	_, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "transfer limit") || !strings.Contains(err.Error(), "does not split") {
		t.Fatalf("expected max-file no-chunking rejection, got %v", err)
	}
}

func TestUpdatePrepareRedirectTrusted(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.redir = "objects.githubusercontent.com"
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err != nil {
		t.Fatalf("trusted redirect should succeed: %v", err)
	}
}

func TestUpdatePrepareRedirectUntrusted(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.redir = "evil.example.com"
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	_, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "untrusted host") {
		t.Fatalf("expected untrusted-redirect rejection, got %v", err)
	}
}

// The current GitHub release-asset redirect host must stay allowed, or real
// downloads break.
func TestUpdatePrepareRedirectReleaseAssetsTrusted(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	f.redir = "release-assets.githubusercontent.com"
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir()); err != nil {
		t.Fatalf("current release-asset redirect host must succeed: %v", err)
	}
}

// Hosts the asset flow never uses are no longer in the allow-list.
func TestUpdatePrepareRedirectUnusedHostsRejected(t *testing.T) {
	for _, host := range []string{"raw.githubusercontent.com", "codeload.github.com", "github.com.evil.example"} {
		f := updateNewFixture(t, "0.6.4")
		f.redir = host
		withFakeTransport(t, f)
		rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
		_, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "untrusted host") {
			t.Fatalf("expected %s redirect rejection, got %v", host, err)
		}
	}
}

func TestUpdatePrepareRemovesTempOnError(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	flipped := append([]byte(nil), f.arch["linux/arm64"]...)
	flipped[5] ^= 0xff
	f.arch["linux/arm64"] = flipped
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	dest := t.TempDir()
	if _, err := updatePrepareAsset(context.Background(), rel, "linux/arm64", dest); err == nil {
		t.Fatal("expected failure")
	}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sparky-update-") {
			t.Fatalf("staging not cleaned: %s", e.Name())
		}
	}
}

func TestUpdatePrepareContextCancelled(t *testing.T) {
	f := updateNewFixture(t, "0.6.4")
	withFakeTransport(t, f)
	rel, _ := updateResolveRelease(context.Background(), "v0.6.4")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := updatePrepareAsset(ctx, rel, "linux/arm64", t.TempDir()); err == nil {
		t.Fatal("expected cancellation error")
	}
}
