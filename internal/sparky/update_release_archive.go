package sparky

// Verified-release download, extraction and per-binary validation for
// "sparky update". Everything here refuses to execute what it downloads; the
// caller moves the prepared binaries out of the returned directory itself.
//
// The security posture of Prepare:
//
//   * The requested archive is downloaded once, into a private 0700 staging
//     tree, over the bounded HTTPS client defined in update_release.go.
//   * The compressed digest and size are verified against the already-resolved
//     release BEFORE any byte is unpacked, so a corrupt or substituted
//     archive never reaches the extractor.
//   * The tar reader is strict: only the two CLI binaries, BUILDINFO.json and
//     an optional LICENSE under the one expected top directory are accepted.
//     Traversal, absolute paths, symlinks, hardlinks, device/fifo/socket
//     entries, duplicates, unexpected files, oversized files and an entry-count
//     flood all fail closed.
//   * Every extracted binary is re-hashed and required to match BOTH manifest
//     sources (release-metadata.json binary_sha256 and BUILDINFO.json
//     binaries[]), and to be a real ARM64 executable object.
//   * Any single regular file above the 16 MiB node-transfer cap fails clearly;
//     nothing here chunks or splits a file.
//   * Temporary files this library creates are removed on any error.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// updateMaxArchiveBytes bounds the compressed download body. A Sparky CLI
	// archive is well under this; anything larger is refused while streaming.
	updateMaxArchiveBytes = 128 << 20
	// updateMaxUnpackedBytes bounds the total bytes the gzip+tar reader may
	// produce, defeating a decompression bomb before it fills the disk.
	updateMaxUnpackedBytes = 96 << 20
	// updateMaxEntries bounds the tar member count.
	updateMaxEntries = 128
	// updateMaxBuildInfoBytes bounds BUILDINFO.json when read from the archive.
	updateMaxBuildInfoBytes = 64 << 10
)

// updateBuildInfoDoc mirrors BUILDINFO.json as tools/release-bundle.sh writes it.
type updateBuildInfoDoc struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Tag      string `json:"tag"`
	Commit   string `json:"commit"`
	Platform string `json:"platform"`
	Binaries []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"binaries"`
}

// updatePrepareAsset downloads and verifies exactly one platform archive from a
// previously resolved release, returning the private staging directory that
// holds bin/sparky, bin/sparky-install and BUILDINFO.json for that platform.
//
// dest is the base directory under which the private 0700 staging tree is
// created; an empty dest uses the system temporary directory. On any error the
// staging tree this call created is removed. Nothing here executes a binary.
func updatePrepareAsset(ctx context.Context, rel *updateRelease, platform, dest string) (*updatePrepared, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rel == nil {
		return nil, updateReleaseError("No resolved release was supplied.")
	}
	osName, ok := updatePlatformOS(platform)
	if !ok {
		return nil, updateReleaseError("Unsupported update platform %q.", platform)
	}
	asset, ok := rel.Assets[platform]
	if !ok || asset.URL == "" {
		return nil, updateReleaseError("The resolved release has no archive for %s.", platform)
	}
	if !isHexDigest(asset.SHA256) || asset.Size <= 0 {
		return nil, updateReleaseError("The resolved archive for %s has an invalid digest or size.", platform)
	}

	base := strings.TrimSpace(dest)
	if base == "" {
		base = os.TempDir()
	}
	staging, err := os.MkdirTemp(base, "sparky-update-")
	if err != nil {
		return nil, updateReleaseError("Could not create a private staging directory.")
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		os.RemoveAll(staging)
		return nil, updateReleaseError("Could not protect the staging directory.")
	}

	// Clean up everything this call created unless the caller receives a
	// successfully prepared tree.
	prepared := false
	defer func() {
		if !prepared {
			os.RemoveAll(staging)
		}
	}()

	archivePath := filepath.Join(staging, asset.Name)
	if err := updateDownloadArchive(ctx, asset.URL, archivePath, asset.Size, asset.SHA256); err != nil {
		return nil, err
	}

	top := "sparky-" + rel.Version + "-" + osName + "-arm64"
	if err := updateExtractArchive(archivePath, staging, top); err != nil {
		return nil, err
	}

	topDir := filepath.Join(staging, top)
	binDir := filepath.Join(topDir, "bin")
	cli := filepath.Join(binDir, "sparky")
	installer := filepath.Join(binDir, "sparky-install")
	buildInfoPath := filepath.Join(topDir, "BUILDINFO.json")

	info, err := updateReadBuildInfo(buildInfoPath)
	if err != nil {
		return nil, err
	}
	if info.Name != "" && info.Name != "sparky" {
		return nil, updateReleaseError("BUILDINFO.json names an unexpected product.")
	}
	if info.Version != rel.Version {
		return nil, updateReleaseError("BUILDINFO.json version %q does not match the release %q.", info.Version, rel.Version)
	}
	if info.Tag != rel.Tag {
		return nil, updateReleaseError("BUILDINFO.json tag %q does not match the release tag %q.", info.Tag, rel.Tag)
	}
	if !strings.EqualFold(info.Commit, rel.Commit) {
		return nil, updateReleaseError("BUILDINFO.json commit %q does not match the release commit.", info.Commit)
	}
	if info.Platform != platform {
		return nil, updateReleaseError("BUILDINFO.json platform %q does not match the requested %q.", info.Platform, platform)
	}
	buildInfoDigests := map[string]string{}
	for _, b := range info.Binaries {
		cleanPath := path.Clean(b.Path)
		if cleanPath != b.Path || !strings.HasPrefix(b.Path, "bin/") {
			return nil, updateReleaseError("BUILDINFO.json lists an invalid binary path.")
		}
		if !isHexDigest(b.SHA256) {
			return nil, updateReleaseError("BUILDINFO.json lists an invalid binary digest.")
		}
		buildInfoDigests[strings.TrimPrefix(cleanPath, "bin/")] = strings.ToLower(b.SHA256)
	}

	wantBinaries := rel.BinarySHA256[platform]
	if len(wantBinaries) != 2 {
		return nil, updateReleaseError("The resolved release has no per-binary hashes for %s; this release predates the sparky updater and must be installed manually.", platform)
	}

	cliSHA, err := updateVerifyBinary(cli, "sparky", osName, wantBinaries, buildInfoDigests)
	if err != nil {
		return nil, err
	}
	installerSHA, err := updateVerifyBinary(installer, "sparky-install", osName, wantBinaries, buildInfoDigests)
	if err != nil {
		return nil, err
	}

	prepared = true
	return &updatePrepared{
		Dir:          topDir,
		CLI:          cli,
		Installer:    installer,
		CLISHA:       cliSHA,
		InstallerSHA: installerSHA,
	}, nil
}

// updateDownloadArchive streams the asset to dest, verifying size and SHA-256
// before returning. The body is capped during streaming so a lying server
// cannot exhaust the disk, and a cancelled context tears the request down.
func updateDownloadArchive(ctx context.Context, url, dest string, wantSize int64, wantSHA string) error {
	if !strings.HasPrefix(url, "https://") {
		return updateReleaseError("Release download requires an HTTPS URL.")
	}
	host := updateURLHost(url)
	if host == "" || !updateTrustedHost(host) {
		return updateReleaseError("Release download target %q is not a trusted host.", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return updateReleaseError("Could not build the download request.")
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "sparky-update")
	resp, err := updateNewHTTPClient(5 * time.Minute).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return updateReleaseError("Could not reach the release server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return updateStatusError(resp.StatusCode, resp.Header)
	}
	if resp.ContentLength > updateMaxArchiveBytes {
		return updateReleaseError("The archive is larger than allowed.")
	}

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return updateReleaseError("Could not open the staging archive.")
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(resp.Body, updateMaxArchiveBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return updateReleaseError("Could not download the release archive: %v", copyErr)
	}
	if closeErr != nil {
		return updateReleaseError("Could not finish writing the release archive.")
	}
	if written > updateMaxArchiveBytes {
		return updateReleaseError("The archive is larger than allowed.")
	}
	if written != wantSize {
		return updateReleaseError("The downloaded archive size %d does not match the expected %d.", written, wantSize)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, wantSHA) {
		return updateReleaseError("The downloaded archive digest does not match the release metadata.")
	}
	return nil
}

// updateExtractArchive unpacks the verified archive into staging under exactly
// one expected top directory, rejecting every unsafe entry. It returns only
// after the required files are present and correctly typed.
func updateExtractArchive(archivePath, staging, top string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return updateReleaseError("Could not open the downloaded archive.")
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return updateReleaseError("The archive is not a valid gzip stream.")
	}
	defer gz.Close()
	gz.Multistream(false)

	reader := &updateCountingReader{r: gz, limit: updateMaxUnpackedBytes}
	tr := tar.NewReader(reader)

	seen := map[string]bool{}
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return updateReleaseError("The archive is malformed: %v", err)
		}
		count++
		if count > updateMaxEntries {
			return updateReleaseError("The archive contains too many entries.")
		}
		name := hdr.Name
		if !updateSafeEntryName(name, top) {
			return updateReleaseError("The archive contains an unsafe path.")
		}
		if seen[name] {
			return updateReleaseError("The archive contains a duplicate entry.")
		}
		seen[name] = true

		switch hdr.Typeflag {
		case tar.TypeDir:
			if name != top && name != top+"/bin" && name != top+"/bin/" && name != top+"/" {
				return updateReleaseError("The archive contains an unexpected directory.")
			}
			if err := os.MkdirAll(filepath.Join(staging, path.Clean(name)), 0o700); err != nil {
				return updateReleaseError("Could not create the staging layout.")
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
			// regular files are handled below
		default:
			// Symlinks, hardlinks, character/block/fifo/socket entries and any
			// other type are rejected outright.
			return updateReleaseError("The archive contains a non-regular entry.")
		}

		if hdr.Size < 0 || hdr.Size > deploymentMaxPayload {
			return updateReleaseError("An archive file is larger than the node transfer limit; it cannot be transferred to a node, and this tool does not split files.")
		}
		rel := strings.TrimPrefix(path.Clean(name), top+"/")
		if !updateExpectedFile(rel) {
			return updateReleaseError("The archive contains an unexpected file.")
		}
		if err := updateWriteMember(tr, staging, path.Clean(name), rel, hdr); err != nil {
			return err
		}
	}

	for _, want := range []string{"bin/sparky", "bin/sparky-install", "BUILDINFO.json"} {
		if !seen[top+"/"+want] {
			return updateReleaseError("The archive is missing %s.", want)
		}
	}
	return nil
}

// updateWriteMember writes one regular member under staging, enforcing the
// per-file cap while copying and setting a mode appropriate to its role.
func updateWriteMember(tr io.Reader, staging, cleanName, rel string, hdr *tar.Header) error {
	target := filepath.Join(staging, filepath.FromSlash(cleanName))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return updateReleaseError("Could not create the staging layout.")
	}
	mode := os.FileMode(0o644)
	if rel == "bin/sparky" || rel == "bin/sparky-install" {
		// The tar header must itself carry an owner-executable bit for the two
		// binaries; a non-executable source binary is refused.
		if hdr.Mode&0o100 == 0 {
			return updateReleaseError("An archive binary is not marked executable.")
		}
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return updateReleaseError("The archive contains a duplicate entry.")
		}
		return updateReleaseError("Could not write an extracted file.")
	}
	n, err := io.Copy(out, io.LimitReader(tr, deploymentMaxPayload+1))
	closeErr := out.Close()
	if err != nil {
		return updateReleaseError("Could not extract an archive file: %v", err)
	}
	if closeErr != nil {
		return updateReleaseError("Could not finish writing an archive file.")
	}
	if n > deploymentMaxPayload {
		return updateReleaseError("An archive file is larger than the node transfer limit; it cannot be transferred to a node, and this tool does not split files.")
	}
	if n != hdr.Size {
		return updateReleaseError("An archive file does not match its declared size.")
	}
	if err := os.Chmod(target, mode); err != nil {
		return updateReleaseError("Could not set file permissions on an extracted file.")
	}
	return nil
}

// updateSafeEntryName accepts only clean, relative paths that live inside the
// single expected top directory, with no traversal, absolute prefix, leading
// dot-slash, backslash or NUL tricks.
func updateSafeEntryName(name, top string) bool {
	if name == "" || strings.ContainsRune(name, '\\') || strings.IndexByte(name, 0) >= 0 {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "./") {
		return false
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || trimmed == "." {
		return false
	}
	if path.Clean(trimmed) != trimmed {
		return false
	}
	if path.IsAbs(trimmed) || strings.HasPrefix(trimmed, "../") || trimmed == ".." {
		return false
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	if trimmed != top && !strings.HasPrefix(trimmed, top+"/") {
		return false
	}
	return true
}

// updateExpectedFile reports whether a path relative to the top directory is one
// of the files this library installs.
func updateExpectedFile(rel string) bool {
	switch rel {
	case "bin/sparky", "bin/sparky-install", "BUILDINFO.json", "LICENSE":
		return true
	}
	return false
}

// updateReadBuildInfo reads and parses BUILDINFO.json with a strict size cap.
func updateReadBuildInfo(p string) (*updateBuildInfoDoc, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, updateReleaseError("The archive has no BUILDINFO.json.")
	}
	if !info.Mode().IsRegular() || info.Size() > updateMaxBuildInfoBytes {
		return nil, updateReleaseError("BUILDINFO.json is not a plain, reasonably sized file.")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, updateReleaseError("Could not read BUILDINFO.json.")
	}
	var doc updateBuildInfoDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, updateReleaseError("BUILDINFO.json is malformed: %v", err)
	}
	return &doc, nil
}

// updateVerifyBinary re-hashes one extracted binary, requires it to match BOTH
// manifest sources, checks the executable permission bit, and confirms it is a
// real ARM64 executable object for the platform. It returns the lowercase
// digest of the bytes on disk. Nothing is executed.
func updateVerifyBinary(p, name, osName string, want, buildInfo map[string]string) (string, error) {
	wantMeta, ok := want[name]
	if !ok || !isHexDigest(wantMeta) {
		return "", updateReleaseError("The release metadata has no valid hash for bin/%s.", name)
	}
	wantBI, ok := buildInfo[name]
	if !ok {
		return "", updateReleaseError("BUILDINFO.json lists no hash for bin/%s.", name)
	}
	if !strings.EqualFold(wantMeta, wantBI) {
		return "", updateReleaseError("The two release manifests disagree on the hash for bin/%s.", name)
	}

	st, err := os.Lstat(p)
	if err != nil {
		return "", updateReleaseError("The archive is missing bin/%s.", name)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return "", updateReleaseError("bin/%s is not a regular file.", name)
	}
	if st.Mode().Perm()&0o100 == 0 {
		return "", updateReleaseError("bin/%s is not executable.", name)
	}
	if st.Size() > deploymentMaxPayload {
		return "", updateReleaseError("bin/%s is larger than the node transfer limit; it cannot be transferred, and this tool does not split files.", name)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		return "", updateReleaseError("Could not read bin/%s.", name)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantMeta) {
		return "", updateReleaseError("bin/%s does not match its expected hash.", name)
	}
	if err := updateCheckExecutableObject(raw, osName); err != nil {
		return "", updateReleaseError("bin/%s is not a valid ARM64 executable for %s: %v", name, osName, err)
	}
	return got, nil
}

// updateCheckExecutableObject parses the raw bytes as the platform's executable
// format and requires a real, ARM64, executable object. debug/elf and
// debug/macho parse either format on any host, so a linux archive is validated
// correctly even when Prepare runs on macOS.
func updateCheckExecutableObject(raw []byte, osName string) error {
	switch osName {
	case "linux":
		f, err := elf.NewFile(bytes.NewReader(raw))
		if err != nil {
			return updateReleaseError("not a valid ELF file")
		}
		defer f.Close()
		switch f.Type {
		case elf.ET_EXEC:
		case elf.ET_DYN:
			interp := false
			for _, prog := range f.Progs {
				if prog.Type == elf.PT_INTERP {
					interp = true
					break
				}
			}
			if !interp {
				return updateReleaseError("ELF is a shared object, not an executable")
			}
		default:
			return updateReleaseError("ELF type is not an executable")
		}
		if f.Machine != elf.EM_AARCH64 {
			return updateReleaseError("ELF architecture is not ARM64")
		}
		return nil
	case "darwin":
		f, err := macho.NewFile(bytes.NewReader(raw))
		if err != nil {
			return updateReleaseError("not a valid Mach-O file")
		}
		defer f.Close()
		if f.Type != macho.TypeExec {
			return updateReleaseError("Mach-O type is not an executable")
		}
		if f.Cpu != macho.CpuArm64 {
			return updateReleaseError("Mach-O architecture is not ARM64")
		}
		return nil
	default:
		return updateReleaseError("unsupported platform")
	}
}

// updateCountingReader caps the total bytes produced by a decompression stream
// so a bomb cannot inflate past the limit.
type updateCountingReader struct {
	r     io.Reader
	read  int64
	limit int64
}

func (c *updateCountingReader) Read(p []byte) (int, error) {
	if c.read >= c.limit {
		// One extra read surfaces an over-limit stream as an error rather than a
		// clean EOF that the tar loop would otherwise treat as a short archive.
		return 0, updateReleaseError("The archive decompresses beyond the allowed size.")
	}
	if int64(len(p)) > c.limit-c.read {
		p = p[:int(c.limit-c.read)]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}
