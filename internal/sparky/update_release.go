package sparky

// Verified-release library for "sparky update".
//
// This file resolves a published GitHub release for the fixed public repository
// ajaykumarns/spark-launcher and verifies its small metadata before any large
// download happens. The archive download, safe extraction and per-binary checks
// live in update_release_archive.go.
//
// Trust model (read this before wiring a caller):
//
//   * Everything is anchored on GitHub's TLS transport for the fixed hosts
//     api.github.com and github.com. There are no independent OpenPGP
//     signatures here. The archive digest, the SHA256SUMS digest and the
//     per-binary digests all share that single GitHub trust: an attacker who
//     controls the release can rewrite them consistently. The digests are
//     integrity checks against corruption and casual tampering in transit, not
//     a substitute for publisher authentication.
//   * Resolve only ever fetches small JSON/checksum documents. It writes
//     nothing to disk and never opens the archives.
//   * Prepare downloads exactly one platform archive into a private 0700
//     staging tree, verifies the compressed digest/size BEFORE extraction and
//     then unpacks with a strict reader that rejects anything unsafe.
//   * Nothing in this library executes a downloaded binary.
//
// The first release that carries the top-level binary_sha256 object is v0.6.4.
// An older target release has no such object, so Resolve fails with a clear
// "predates the updater" error pointing the operator at a manual install.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// updateReleaseRepo is the only repository any resolver or downloader here can
// ever talk to. It is a constant on purpose: no caller may steer the library at
// an arbitrary host, fork or mirror.
const updateReleaseRepo = "ajaykumarns/spark-launcher"

// Production base URLs stay fixed. The seams below exist so tests can point the
// two fetch helpers at a fake transport; nothing user-facing feeds them.
var (
	updateReleaseAPIBase     = "https://api.github.com"
	updateReleaseDownloadURL = "https://github.com"
	// updateReleaseTransport is the effect seam for HTTP. Production uses the
	// bounded default below; tests install a fake RoundTripper so no test ever
	// opens a socket.
	updateReleaseTransport http.RoundTripper
)

// updateMaxRedirects bounds the dedicated downloader. GitHub release downloads
// are handed off to a githubusercontent.com object store, so redirects are
// required, but the hop count is capped and every hop is host-checked.
const updateMaxRedirects = 5

// Bounded caps for the response bodies this library is willing to read. The
// metadata documents are tiny; anything near a cap is a malformed or hostile
// response and is refused before the bytes are decoded.
const (
	updateMaxReleaseJSON = 1 << 20   // release API document
	updateMaxMetadata    = 1 << 20   // release-metadata.json
	updateMaxChecksums   = 512 << 10 // SHA256SUMS
	updateMaxChecksumKey = 256 << 10 // per-checksum helper bound
	updateMaxBodySlack   = 1 << 10   // allowance over Content-Length before we bail
)

// updateTrustedHost is the only host a request or any of its redirects may
// target. Anything else is rejected before a connection is made. The list is
// deliberately the minimum the fixed-repo asset flow actually needs: the API
// host, the github.com download host, and the asset-redirect object stores
// (release-assets.githubusercontent.com is where GitHub sends release assets
// today; objects- and github-releases- are kept so an older hop still works).
// Hosts this library never talks to - raw file serving, codeload, arbitrary
// mirrors - are not allowed.
func updateTrustedHost(host string) bool {
	switch host {
	case "api.github.com", "github.com",
		"release-assets.githubusercontent.com",
		"objects.githubusercontent.com",
		"github-releases.githubusercontent.com":
		return true
	}
	return false
}

// updateAsset is one release asset resolved to a fully constructed official
// download URL. URL is never taken from a caller or from untrusted text: it is
// built by updateAssetURL from the fixed repo, the verified tag and the
// allow-listed asset name.
type updateAsset struct {
	Name   string
	URL    string
	SHA256 string
	Size   int64
}

// updateRelease is a verified release. Assets is keyed by "os/arch" and holds
// only the CLI+installer archives this library knows how to install.
// BinarySHA256[platform][name] carries the expected digests of the extracted
// binaries ("sparky" and "sparky-install"); those are what "sparky update
// --check" reads, so a check never has to open an archive.
type updateRelease struct {
	Tag          string
	Version      string
	Commit       string
	Assets       map[string]updateAsset
	BinarySHA256 map[string]map[string]string
}

// updatePrepared is the result of a verified download+extraction: the private
// staging directory holding the platform top tree with bin/sparky,
// bin/sparky-install and BUILDINFO.json, plus absolute paths and digests of the
// two binaries. The caller moves the binaries out of Dir; this library never
// executes them.
type updatePrepared struct {
	Dir          string
	CLI          string
	Installer    string
	CLISHA       string
	InstallerSHA string
}

// updateReleaseDoc mirrors the fields of release-metadata.json this library
// trusts. Unknown fields are ignored; every field it does read is validated.
// Nothing from this document is ever executed or used to build a URL.
type updateReleaseDoc struct {
	Name            string   `json:"name"`
	Tag             string   `json:"tag"`
	Version         string   `json:"version"`
	Commit          string   `json:"commit"`
	Platforms       []string `json:"platforms"`      // must be exactly updateSupportedPlatforms
	ChecksumsFile   string   `json:"checksums_file"` // must be updateChecksumsFileName
	ChecksumsSHA256 string   `json:"checksums_sha256"`
	Assets          []struct {
		File      string `json:"file"`
		Platform  string `json:"platform"`
		Kind      string `json:"kind"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"assets"`
	// BinarySHA256 is the top-level per-binary digest map added in v0.6.4. It
	// is absent on older releases, which the caller must report as predating
	// the updater rather than as a hash mismatch.
	BinarySHA256 map[string]map[string]string `json:"binary_sha256"`
}

// updateReleaseAPI mirrors the GitHub releases API fields we consult. Draft and
// prerelease are read explicitly so a half-published release fails closed.
type updateReleaseAPI struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name            string `json:"name"`
		Digest          string `json:"digest"`
		BrowserDownload string `json:"browser_download_url"`
		Size            int64  `json:"size"`
	} `json:"assets"`
}

// updateReleaseCommitAPI mirrors the tag reference API used to tie a tag to a
// full commit hash.
type updateReleaseCommitAPI struct {
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// updateReleaseError carries a human-readable failure. Every message is written
// for an operator: it names the thing that is wrong and never leaks a secret.
func updateReleaseError(format string, args ...any) error {
	return fail(format, args...)
}

// isHexDigest reports whether s is exactly 64 lowercase hexadecimal characters,
// the shape of a SHA-256 digest. Unlike the general isHex helper it also pins
// the length and rejects uppercase, so it is safe as a digest gate.
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// isFullCommit reports whether s is exactly 40 lowercase hexadecimal characters,
// the shape of a full Git commit hash.
func isFullCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// updateAssetURL builds the official download URL for one allow-listed asset.
// It never trusts a URL that came from a metadata document: the tag is the
// verified tag, and name is one this package constructed from the version.
func updateAssetURL(tag, name string) (string, error) {
	if tag == "" || strings.ContainsAny(tag, "/\\?# \t\n") || !strings.HasPrefix(tag, "v") {
		return "", updateReleaseError("Release tag %q is not a safe download path segment.", tag)
	}
	if name == "" || strings.ContainsAny(name, "/\\?# \t\n") || strings.HasPrefix(name, ".") {
		return "", updateReleaseError("Asset name %q is not a safe download path segment.", name)
	}
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", updateReleaseDownloadURL, updateReleaseRepo, tag, name), nil
}

// updateSupportedPlatforms is the closed set of "os/arch" platforms a Sparky
// release publishes and this library is able to install.
var updateSupportedPlatforms = []string{"darwin/arm64", "linux/arm64"}

// updateChecksumsFileName is the literal checksums asset name the publisher
// writes, the metadata is required to name, and the fetcher requests.
const updateChecksumsFileName = "SHA256SUMS"

// updateArchiveName is the fixed archive filename for a version and platform.
func updateArchiveName(version, platform string) (string, bool) {
	osName, ok := updatePlatformOS(platform)
	if !ok {
		return "", false
	}
	return "sparky-" + version + "-" + osName + "-arm64.tar.gz", true
}

// updatePlatformOS maps an "os/arch" key to the bare OS name, accepting only the
// two platforms a Sparky release publishes.
func updatePlatformOS(platform string) (string, bool) {
	switch platform {
	case "darwin/arm64":
		return "darwin", true
	case "linux/arm64":
		return "linux", true
	}
	return "", false
}

// updateValidatePlatforms requires the metadata platforms array to name exactly
// the supported set. The publisher always emits both entries, so a missing,
// duplicated or unrecognised value is a malformed or hostile document and is
// never something to install from.
func updateValidatePlatforms(platforms []string) error {
	seen := make(map[string]bool, len(updateSupportedPlatforms))
	for _, p := range platforms {
		if _, ok := updatePlatformOS(p); !ok {
			return updateReleaseError("release-metadata.json lists unsupported platform %q.", p)
		}
		if seen[p] {
			return updateReleaseError("release-metadata.json lists platform %q more than once.", p)
		}
		seen[p] = true
	}
	for _, p := range updateSupportedPlatforms {
		if !seen[p] {
			return updateReleaseError("release-metadata.json does not list the supported platform %s.", p)
		}
	}
	return nil
}

// updateNewHTTPClient builds the dedicated bounded HTTPS client. It is separate
// from the shared request() client on purpose: that one disables redirects,
// while GitHub release downloads require them. Redirects here are capped and
// every hop is scheme- and host-checked; the body caps are enforced at read time.
func updateNewHTTPClient(timeout time.Duration) *http.Client {
	transport := updateReleaseTransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > updateMaxRedirects {
				return updateReleaseError("Too many redirects while downloading a release asset.")
			}
			if req.URL.Scheme != "https" {
				return updateReleaseError("Release download refused a non-HTTPS redirect.")
			}
			if !updateTrustedHost(req.URL.Host) {
				return updateReleaseError("Release download redirected to an untrusted host.")
			}
			return nil
		},
	}
}

// updateGet is the single HTTP entry point for this library. It refuses any
// non-HTTPS URL or untrusted host before sending, attaches the context so a
// cancelled caller tears the request down, and caps the response body at
// maxBytes with a small slack over the advertised Content-Length so an inflated
// body is refused without buffering it all.
func updateGet(ctx context.Context, url string, maxBytes int64, accept string) ([]byte, int, http.Header, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, 0, nil, updateReleaseError("Release fetch requires an HTTPS URL.")
	}
	parsedHost := updateURLHost(url)
	if parsedHost == "" || !updateTrustedHost(parsedHost) {
		return nil, 0, nil, updateReleaseError("Release fetch target %q is not a trusted host.", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, nil, updateReleaseError("Could not build the release request.")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "sparky-update")
	resp, err := updateNewHTTPClient(60 * time.Second).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, nil, ctx.Err()
		}
		return nil, 0, nil, updateReleaseError("Could not reach the release server: %v", err)
	}
	defer resp.Body.Close()
	if limit := updateResponseLimit(resp, maxBytes); limit >= 0 {
		// Read one byte past the cap so a body that lies about its length is
		// detected instead of silently truncated.
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, resp.StatusCode, resp.Header, ctx.Err()
			}
			return nil, resp.StatusCode, resp.Header, updateReleaseError("Could not read the release response: %v", rerr)
		}
		if int64(len(body)) > limit {
			return nil, resp.StatusCode, resp.Header, updateReleaseError("The release response is larger than allowed.")
		}
		return body, resp.StatusCode, resp.Header, nil
	}
	return nil, resp.StatusCode, resp.Header, updateResponseStatusError(resp)
}

// updateResponseLimit turns a status into either a byte cap to read (>=0) or a
// negative sentinel meaning the status is already an error.
func updateResponseLimit(resp *http.Response, maxBytes int64) int64 {
	switch {
	case resp.StatusCode == http.StatusOK:
		if resp.ContentLength > maxBytes+updateMaxBodySlack {
			return -1
		}
		return maxBytes
	default:
		return -1
	}
}

// updateResponseStatusError renders an unusable status code, including GitHub's
// rate-limit and reset signals so an operator gets actionable guidance. It
// never claims that --tag or any other flag bypasses the limit.
func updateResponseStatusError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return updateReleaseError("The release was not found on GitHub (HTTP %d).", resp.StatusCode)
	case http.StatusForbidden, http.StatusTooManyRequests:
		detail := "GitHub is rate limiting release requests."
		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
			if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
				if secs, err := strconv.ParseInt(reset, 10, 64); err == nil {
					detail += fmt.Sprintf(" The limit resets at %s.",
						time.Unix(secs, 0).UTC().Format(time.RFC3339))
				}
			}
		}
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			if secs, err := strconv.Atoi(retry); err == nil && secs >= 0 {
				detail += fmt.Sprintf(" Retry in about %ds.", secs)
			}
		}
		detail += " Try again later; specifying a tag does not lift this limit."
		return updateReleaseError("%s (HTTP %d)", detail, resp.StatusCode)
	default:
		return updateReleaseError("Unexpected response from the release server (HTTP %d).", resp.StatusCode)
	}
}

// updateURLHost extracts the host component of an absolute URL without pulling
// in net/url for a single field; it is intentionally conservative.
func updateURLHost(raw string) string {
	rest := strings.TrimPrefix(raw, "https://")
	if rest == raw {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		return "" // userinfo in the authority is never a trusted host
	}
	return rest
}

// updateFetchReleaseAPI fetches the small GitHub releases API document for the
// fixed repo, for either the latest release or an exact tag.
func updateFetchReleaseAPI(ctx context.Context, tag string) (*updateReleaseAPI, error) {
	var url string
	if tag == "" {
		url = fmt.Sprintf("%s/repos/%s/releases/latest", updateReleaseAPIBase, updateReleaseRepo)
	} else {
		if strings.ContainsAny(tag, "/\\?# \t\n") {
			return nil, updateReleaseError("Release tag %q is not a safe path segment.", tag)
		}
		url = fmt.Sprintf("%s/repos/%s/releases/tags/%s", updateReleaseAPIBase, updateReleaseRepo, tag)
	}
	body, status, header, err := updateGet(ctx, url, updateMaxReleaseJSON, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, updateStatusError(status, header)
	}
	var doc updateReleaseAPI
	// One tolerant decode: GitHub adds fields to this document over time, so
	// unknown keys are ignored instead of failing the release. The body is
	// already size-capped by updateGet, and every field this library relies on
	// (tag, draft/prerelease, asset digests) is validated by the caller.
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, updateReleaseError("The GitHub release API returned malformed JSON: %v", err)
	}
	return &doc, nil
}

// updateFetchTagCommit resolves a tag to the full commit hash it points at, so
// the release metadata commit can be required to agree with the tag itself.
func updateFetchTagCommit(ctx context.Context, tag string) (string, error) {
	if strings.ContainsAny(tag, "/\\?# \t\n") {
		return "", updateReleaseError("Release tag %q is not a safe path segment.", tag)
	}
	url := fmt.Sprintf("%s/repos/%s/git/ref/tags/%s", updateReleaseAPIBase, updateReleaseRepo, tag)
	body, status, header, err := updateGet(ctx, url, updateMaxReleaseJSON, "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", updateStatusError(status, header)
	}
	var ref updateReleaseCommitAPI
	if err := json.Unmarshal(body, &ref); err != nil {
		return "", updateReleaseError("The GitHub tag API returned malformed JSON: %v", err)
	}
	sha := ref.Object.SHA
	// A lightweight tag points straight at a commit. An annotated tag points at
	// a tag object whose SHA is not a commit; the caller still learns the tag
	// object sha but we require a full commit hash, so annotated tags resolve
	// through the tag-object endpoint.
	if strings.EqualFold(ref.Object.Type, "tag") {
		if !isFullCommit(sha) {
			return "", updateReleaseError("GitHub returned a tag object that is not a valid commit reference.")
		}
		turl := fmt.Sprintf("%s/repos/%s/git/tags/%s", updateReleaseAPIBase, updateReleaseRepo, sha)
		tbody, tstatus, theader, terr := updateGet(ctx, turl, updateMaxReleaseJSON, "application/vnd.github+json")
		if terr != nil {
			return "", terr
		}
		if tstatus != http.StatusOK {
			return "", updateStatusError(tstatus, theader)
		}
		var tagObj updateReleaseCommitAPI
		if err := json.Unmarshal(tbody, &tagObj); err != nil {
			return "", updateReleaseError("The GitHub tag-object API returned malformed JSON: %v", err)
		}
		sha = tagObj.Object.SHA
	}
	if !isFullCommit(sha) {
		return "", updateReleaseError("GitHub returned a tag commit that is not a full 40-character hash.")
	}
	return strings.ToLower(sha), nil
}

// updateFetchMetadata fetches and safely parses release-metadata.json.
func updateFetchMetadata(ctx context.Context, tag string) (*updateReleaseDoc, error) {
	name := "release-metadata.json"
	url, err := updateAssetURL(tag, name)
	if err != nil {
		return nil, err
	}
	body, status, header, err := updateGet(ctx, url, updateMaxMetadata, "application/json")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, updateStatusError(status, header)
	}
	var doc updateReleaseDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, updateReleaseError("release-metadata.json is malformed: %v", err)
	}
	return &doc, nil
}

// updateFetchChecksums fetches SHA256SUMS and returns its raw bytes plus the
// map of filename -> lowercase digest parsed from it. The file itself is
// verified against the metadata digest by the caller.
func updateFetchChecksums(ctx context.Context, tag string) ([]byte, map[string]string, error) {
	url, err := updateAssetURL(tag, updateChecksumsFileName)
	if err != nil {
		return nil, nil, err
	}
	body, status, header, err := updateGet(ctx, url, updateMaxChecksums, "text/plain")
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, nil, updateStatusError(status, header)
	}
	sums, err := updateParseChecksums(body)
	if err != nil {
		return nil, nil, err
	}
	return body, sums, nil
}

// updateParseChecksums parses "<hex>  <name>" lines. Every digest is validated
// for shape; a malformed line fails the whole file rather than being skipped.
func updateParseChecksums(body []byte) (map[string]string, error) {
	if int64(len(body)) > updateMaxChecksumKey {
		return nil, updateReleaseError("SHA256SUMS is larger than allowed.")
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		digest, name, ok := strings.Cut(line, " ")
		if !ok {
			return nil, updateReleaseError("SHA256SUMS has a malformed line.")
		}
		name = strings.TrimLeft(name, " *") // tolerate the binary-mode marker
		if !isHexDigest(digest) {
			return nil, updateReleaseError("SHA256SUMS has an invalid digest for %q.", name)
		}
		if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
			return nil, updateReleaseError("SHA256SUMS has an invalid file name.")
		}
		out[name] = strings.ToLower(digest)
	}
	if len(out) == 0 {
		return nil, updateReleaseError("SHA256SUMS is empty.")
	}
	return out, nil
}

// updateStatusError is the helper form used by the fetch wrappers.
func updateStatusError(status int, header http.Header) error {
	return updateResponseStatusError(&http.Response{StatusCode: status, Header: header})
}

// updateResolveRelease resolves and verifies a release without downloading any
// archive or writing any file. tag=="" resolves the latest release; a specific
// tag resolves exactly that release. Rate limits surface as actionable errors;
// no flag bypasses them.
func updateResolveRelease(ctx context.Context, tag string) (*updateRelease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	api, err := updateFetchReleaseAPI(ctx, tag)
	if err != nil {
		return nil, err
	}
	if api.Draft || api.Prerelease {
		return nil, updateReleaseError("Draft and prerelease builds are not installable updates.")
	}
	resolvedTag := strings.TrimSpace(api.TagName)
	if resolvedTag == "" {
		return nil, updateReleaseError("GitHub returned a release with no tag.")
	}
	if tag != "" && resolvedTag != tag {
		return nil, updateReleaseError("GitHub returned tag %q for the requested %q.", resolvedTag, tag)
	}
	version, ok := strings.CutPrefix(resolvedTag, "v")
	if !ok || !isReleaseVersion(version) {
		return nil, updateReleaseError("Release tag %q is not of the form vX.Y.Z.", resolvedTag)
	}

	metadata, err := updateFetchMetadata(ctx, resolvedTag)
	if err != nil {
		return nil, err
	}
	if metadata.Tag != resolvedTag {
		return nil, updateReleaseError("release-metadata.json tag %q does not match the release tag %q.", metadata.Tag, resolvedTag)
	}
	if metadata.Version != version {
		return nil, updateReleaseError("release-metadata.json version %q does not match the release tag %q.", metadata.Version, resolvedTag)
	}
	if !isFullCommit(metadata.Commit) {
		return nil, updateReleaseError("release-metadata.json does not record a full 40-character commit.")
	}
	// The two small contract fields the publisher always emits are required to
	// say exactly what this library expects before any further fetch: the
	// checksums asset is fetched by a fixed name, and the platform list must
	// match the set this library installs.
	if metadata.ChecksumsFile != updateChecksumsFileName {
		return nil, updateReleaseError("release-metadata.json names checksums file %q, expected %q.", metadata.ChecksumsFile, updateChecksumsFileName)
	}
	if err := updateValidatePlatforms(metadata.Platforms); err != nil {
		return nil, err
	}

	// Tie the metadata commit to the tag itself. The tag reference and the
	// metadata must name the same commit; this is a same-origin consistency
	// check, not an independent signature.
	tagCommit, err := updateFetchTagCommit(ctx, resolvedTag)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(tagCommit, metadata.Commit) {
		return nil, updateReleaseError("The release tag points at commit %s but release-metadata.json claims %s.", tagCommit, metadata.Commit)
	}

	// The checksums file must be exactly the bytes the metadata digests.
	sumsRaw, sums, err := updateFetchChecksums(ctx, resolvedTag)
	if err != nil {
		return nil, err
	}
	if !isHexDigest(metadata.ChecksumsSHA256) {
		return nil, updateReleaseError("release-metadata.json does not record a valid checksums digest.")
	}
	if sum := sha256Hex(sumsRaw); !strings.EqualFold(sum, metadata.ChecksumsSHA256) {
		return nil, updateReleaseError("SHA256SUMS digest %s does not match release-metadata.json %s.", sum, metadata.ChecksumsSHA256)
	}

	// Per-binary digests are mandatory for an updater target. Their absence
	// means the release predates the updater.
	if len(metadata.BinarySHA256) == 0 {
		return nil, updateReleaseError("Release %s predates the sparky updater: it publishes no per-binary hashes. Install it manually.", resolvedTag)
	}

	// Index the metadata asset records by platform, keeping only the CLI archive
	// kinds this library installs.
	type metaAsset struct {
		name   string
		sha    string
		size   int64
		exists bool
	}
	byPlatform := map[string]metaAsset{}
	for _, a := range metadata.Assets {
		if a.Kind != "cli-and-installer" {
			continue
		}
		want, ok := updateArchiveName(version, a.Platform)
		if !ok || a.File != want {
			// An asset naming a platform or file we do not recognise is
			// ignored here and caught by the per-platform completeness check
			// below, so a stray asset cannot inject a URL.
			continue
		}
		byPlatform[a.Platform] = metaAsset{name: a.File, sha: a.SHA256, size: a.SizeBytes, exists: true}
	}

	// Index the GitHub API assets so an API-provided digest, when present, is
	// required to agree with the metadata.
	apiDigest := map[string]string{}
	for _, a := range api.Assets {
		if strings.HasPrefix(a.Digest, "sha256:") {
			apiDigest[a.Name] = strings.ToLower(strings.TrimPrefix(a.Digest, "sha256:"))
		}
	}

	rel := &updateRelease{
		Tag:          resolvedTag,
		Version:      version,
		Commit:       strings.ToLower(metadata.Commit),
		Assets:       map[string]updateAsset{},
		BinarySHA256: map[string]map[string]string{},
	}
	for _, platform := range updateSupportedPlatforms {
		want, _ := updateArchiveName(version, platform)
		rec := byPlatform[platform]
		if !rec.exists {
			return nil, updateReleaseError("release-metadata.json has no CLI archive for %s.", platform)
		}
		if !isHexDigest(rec.sha) {
			return nil, updateReleaseError("release-metadata.json has an invalid archive digest for %s.", want)
		}
		if rec.size <= 0 {
			return nil, updateReleaseError("release-metadata.json has an invalid archive size for %s.", want)
		}
		// The archive must appear in SHA256SUMS with the same digest.
		if sum, ok := sums[want]; !ok || !strings.EqualFold(sum, rec.sha) {
			return nil, updateReleaseError("SHA256SUMS does not confirm the archive digest for %s.", want)
		}
		// If GitHub supplied a digest for the asset it must agree too.
		if d, ok := apiDigest[want]; ok && !strings.EqualFold(d, rec.sha) {
			return nil, updateReleaseError("The GitHub asset digest disagrees with release-metadata.json for %s.", want)
		}
		url, err := updateAssetURL(resolvedTag, want)
		if err != nil {
			return nil, err
		}
		rel.Assets[platform] = updateAsset{Name: want, URL: url, SHA256: strings.ToLower(rec.sha), Size: rec.size}

		// Both binaries for this platform must carry valid digests. Their
		// absence is a predates-the-updater signal surfaced above; here we
		// only require the shape of what is present.
		binMap, ok := metadata.BinarySHA256[platform]
		if !ok {
			return nil, updateReleaseError("release-metadata.json has no per-binary hashes for %s; this release predates the sparky updater and must be installed manually.", platform)
		}
		clean := map[string]string{}
		for _, bname := range []string{"sparky", "sparky-install"} {
			d, ok := binMap[bname]
			if !ok {
				return nil, updateReleaseError("release-metadata.json has no hash for bin/%s on %s; this release predates the sparky updater and must be installed manually.", bname, platform)
			}
			if !isHexDigest(d) {
				return nil, updateReleaseError("release-metadata.json has an invalid binary digest for bin/%s on %s.", bname, platform)
			}
			clean[bname] = strings.ToLower(d)
		}
		rel.BinarySHA256[platform] = clean
	}
	return rel, nil
}
