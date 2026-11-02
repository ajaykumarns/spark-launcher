// OCR service management on the cluster administrator node, independent of
// LLMs. Actions: install, status, start, stop. Nothing here talks to Python
// Sparky; the spark-ocr engine's own bootstrap and registration script are
// engine-owned and may run in production. Unknown Docker state or corrupt
// metadata must fail; it must never be reported as a stopped service.
package sparky

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// ocrRun is the injectable command runner for the OCR module.
var ocrRun = command

// ocrHealthURL is the local health endpoint of the OCR API container.
const ocrHealthURL = "http://127.0.0.1:8097/health"

// ocrHealth is injectable so tests never open real network connections.
var ocrHealth = func(url string, timeout time.Duration) (Object, error) {
	return requestObject(context.Background(), "GET", url, nil, nil, timeout, false)
}

// ocrStreamer runs a long-lived command and streams its combined output into
// the private installation log instead of buffering it in memory.
type ocrStreamer func(args []string, log io.Writer, timeout time.Duration) error

// ocrStreamRun is the injectable streaming runner for clone, build,
// bootstrap, and registration. The default implementation never buffers the
// full output in RAM and never lets output escape the private log.
var ocrStreamRun ocrStreamer = ocrStreamExec

func ocrStreamExec(args []string, log io.Writer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(log, "%s failed: %v\n", args[0], err)
		return fail("The %s command failed.", args[0])
	}
	return nil
}

// ocrOpenLog opens the private installation log with append-only private
// permissions. O_NOFOLLOW prevents a pre-planted symlink from redirecting
// diagnostics (and anything chained to them) outside the intended file.
var ocrOpenLog = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
}

// ocrLogLine appends one diagnostic line to the open private log; errors are
// surfaced by callers, never silently dropped during setup.
var ocrLogLine = func(f *os.File, line string) error {
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// ocrWriteFile is the OCR-private file-write seam used for installation
// metadata and the stable environment; tests inject failures through it.
var ocrWriteFile = ocrWrite

var ocrActions = []string{"install", "status", "start", "stop"}

type ocrService struct {
	home     string
	root     string
	metadata string
	log      string
}

// newOCRService validates the root before any effect happens.
func newOCRService(root string) (*ocrService, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fail("The cluster root must be an absolute path.")
	}
	home := filepath.Join(root, "ocr")
	return &ocrService{home: home, root: root, metadata: filepath.Join(home, "sparky-install.json")}, nil
}

// ocrRecord reads the installation metadata. A missing file yields an empty
// record; an unreadable or corrupt file is an error, never a default.
func (s *ocrService) ocrRecord() (Object, error) {
	b, err := os.ReadFile(s.metadata)
	if err != nil {
		if os.IsNotExist(err) {
			return Object{}, nil
		}
		return nil, fail("Cannot read the OCR installation metadata.")
	}
	record, err := decode(b)
	if err != nil {
		return nil, fail("The OCR installation metadata is corrupt.")
	}
	return record, nil
}

// ocrSource returns the installed source directory. Missing metadata uses the
// default location; corrupt metadata is an error.
func (s *ocrService) ocrSource() (string, error) {
	record, err := s.ocrRecord()
	if err != nil {
		return "", err
	}
	if src, ok := record["source"]; ok {
		if src == nil {
			return filepath.Join(s.home, "source"), nil
		}
		source := str(src)
		if source == "" || !filepath.IsAbs(source) {
			return "", fail("The OCR installation metadata is corrupt.")
		}
		return source, nil
	}
	return filepath.Join(s.home, "source"), nil
}

// ocrEnvFile prefers the stable private environment and falls back to the
// release's deploy/.env. It never modifies anything.
func (s *ocrService) ocrEnvFile(source string) string {
	stable := filepath.Join(s.home, "service.env")
	if ocrFileExists(stable) {
		return stable
	}
	if source == "" {
		source = filepath.Join(s.home, "source")
	}
	return filepath.Join(source, "deploy", ".env")
}

// ocrInspect asks Docker only for the minimal state/image/project fields and
// never returns the environment, tokens, or health log text. A missing
// container is nil,nil; any other Docker failure is an error.
func (s *ocrService) ocrInspect() (Object, error) {
	template := `{"status":{{json .State.Status}},"running":{{json .State.Running}},` +
		`"health":{{json .State.Health.Status}},"image":{{json .Config.Image}},` +
		`"project":{{json (index .Config.Labels "com.docker.compose.project")}}}`
	result, err := ocrRun([]string{"sudo", "-n", "docker", "inspect", "--format", template, "spark-ocr"}, nil, 30*time.Second)
	if err != nil {
		return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
	}
	if result.Code != 0 {
		// Partial stdout with a nonzero status is still a failure.
		if strings.Contains(strings.ToLower(result.Err), "no such") {
			return nil, nil
		}
		return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
	}
	info, err := decode([]byte(strings.TrimSpace(result.Out)))
	if err != nil {
		return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
	}
	for key := range info {
		switch key {
		case "status", "running", "health", "image", "project":
		default:
			return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
		}
	}
	if str(info["status"]) == "" {
		return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
	}
	if _, ok := info["running"].(bool); !ok {
		return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
	}
	if v := info["image"]; v != nil {
		if _, ok := v.(string); !ok {
			return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
		}
	}
	if v := info["health"]; v != nil {
		if _, ok := v.(string); !ok {
			return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
		}
	}
	if v := info["project"]; v != nil {
		if _, ok := v.(string); !ok {
			return nil, fail("Cannot inspect OCR. Check Docker and administrator access on Spark01.")
		}
	}
	return info, nil
}

// ocrStatus returns the exact public safe fields only. Any unknown state is
// an error rather than a false "stopped".
func (s *ocrService) ocrStatus() (Object, error) {
	info, err := s.ocrInspect()
	if err != nil {
		return nil, err
	}
	source, err := s.ocrSource()
	if err != nil {
		return nil, err
	}
	record, err := s.ocrRecord()
	if err != nil {
		return nil, err
	}
	envFile := s.ocrEnvFile(source)
	installed := ocrFileExists(filepath.Join(source, "deploy", "compose.yaml")) && ocrFileExists(envFile)
	running := false
	status := "not_installed"
	if installed {
		status = "stopped"
	}
	healthStatus := "unknown"
	image := any(nil)
	if info != nil {
		running, _ = info["running"].(bool)
		if s := str(info["status"]); s != "" {
			status = s
		}
		if s := str(info["health"]); s != "" {
			healthStatus = s
		}
		if s := str(info["image"]); s != "" {
			image = s
		}
	}
	apiReady := false
	if running {
		if health, err := ocrHealth(ocrHealthURL, 3*time.Second); err == nil && str(health["service"]) == "spark-ocr" {
			apiReady = true
		}
	}
	return Object{
		"service":   "ocr",
		"host":      "spark01",
		"installed": installed,
		"state":     status,
		"running":   running,
		"health":    healthStatus,
		"api_ready": apiReady,
		"image":     image,
		"revision":  record["revision"],
		"source":    source,
	}, nil
}

// ocrProject resolves and validates the Compose project name.
func (s *ocrService) ocrProject() (string, error) {
	info, err := s.ocrInspect()
	if err != nil {
		return "", err
	}
	project := ""
	if info != nil {
		project = str(info["project"])
	}
	if project == "" {
		record, err := s.ocrRecord()
		if err != nil {
			return "", err
		}
		if raw, ok := record["project"]; ok && raw != nil {
			project = str(raw)
		}
	}
	if project == "" {
		project = "spark-ocr"
	}
	if !ocrProjectName.MatchString(project) {
		return "", fail("Invalid OCR Compose project name.")
	}
	return project, nil
}

var ocrProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func ocrFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func ocrIsTrue(v any) bool {
	b, _ := v.(bool)
	return b
}

// ocrCompose runs docker compose for the OCR project only, keeping the
// existing project name and the private environment file.
func (s *ocrService) ocrCompose(source string, args []string, project string) error {
	if project == "" {
		p, err := s.ocrProject()
		if err != nil {
			return err
		}
		project = p
	}
	envFile := s.ocrEnvFile(source)
	composePath := filepath.Join(source, "deploy", "compose.yaml")
	full := []string{"sudo", "-n", "env", "OCR_SERVICE_ENV_FILE=" + envFile,
		"docker", "compose", "--project-name", project, "--env-file", envFile, "-f", composePath}
	full = append(full, args...)
	result, err := ocrRun(full, nil, 180*time.Second)
	if err != nil || result.Code != 0 {
		return fail("OCR compose command failed.")
	}
	return nil
}

func (s *ocrService) ocrStart(source string, project string) (Object, error) {
	if source == "" {
		src, err := s.ocrSource()
		if err != nil {
			return nil, err
		}
		source = src
	}
	if !ocrFileExists(filepath.Join(source, "deploy", "compose.yaml")) || !ocrFileExists(s.ocrEnvFile(source)) {
		return nil, fail("OCR is not installed. Run sparky ocr install --repo URL first.")
	}
	args := []string{"up", "-d", "--no-build", "--no-deps", "--wait", "--wait-timeout", "90", "ocr"}
	if err := s.ocrCompose(source, args, project); err != nil {
		return nil, err
	}
	result, err := s.ocrStatus()
	if err != nil {
		return nil, err
	}
	if !ocrIsTrue(result["running"]) || !ocrIsTrue(result["api_ready"]) {
		return nil, fail("OCR did not become ready.")
	}
	return result, nil
}

func (s *ocrService) ocrStop() (Object, error) {
	info, err := s.ocrInspect()
	if err != nil {
		return nil, err
	}
	if info != nil && ocrIsTrue(info["running"]) {
		result, err := ocrRun([]string{"sudo", "-n", "docker", "stop", "--time", "30", "spark-ocr"}, nil, 60*time.Second)
		if err != nil || result.Code != 0 {
			return nil, fail("OCR docker command failed.")
		}
	}
	result, err := s.ocrStatus()
	if err != nil {
		return nil, err
	}
	if ocrIsTrue(result["running"]) {
		return nil, fail("OCR is still running.")
	}
	return result, nil
}

// ocrWrite writes text atomically with private permissions, creating the
// parent directory. A temporary file in the destination directory plus rename
// avoids following pre-existing symlinks at the destination path.
func ocrWrite(path string, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fail("Cannot create OCR directories.")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sparky-ocr-*")
	if err != nil {
		return fail("Cannot write OCR files.")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return fail("Cannot write OCR files.")
	}
	if err := tmp.Close(); err != nil {
		return fail("Cannot write OCR files.")
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fail("Cannot write OCR files.")
	}
	if err := os.Rename(name, path); err != nil {
		return fail("Cannot write OCR files.")
	}
	return nil
}

// ocrLock takes the nonblocking control lock. Only one OCR control command
// may run at a time.
func (s *ocrService) ocrLock() (func(), error) {
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return nil, fail("Cannot create OCR directories.")
	}
	fd, err := os.OpenFile(filepath.Join(s.home, "sparky.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fail("Cannot create OCR directories.")
	}
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fd.Close()
		return nil, fail("Another OCR control command is running.")
	}
	return func() { _ = fd.Close() }, nil
}

// ocrValidateRepository accepts credential-free HTTPS (any path, valid port,
// IPv6 hosts) or git@host:path repositories plus a safe ref.
func ocrValidateRepository(repo string, ref string) error {
	if repo == "" || strings.ContainsFunc(repo, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fail("For the first install, give --repo with the private OCR repository URL.")
	}
	if strings.HasPrefix(repo, "git@") {
		if !ocrSSHRepo.MatchString(repo) {
			return fail("Use an HTTPS or git@host:path repository URL without credentials.")
		}
	} else {
		parsed, err := url.Parse(repo)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fail("Use an HTTPS or git@host:path repository URL without credentials.")
		}
		if port := parsed.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 || (len(port) > 1 && port[0] == '0') {
				return fail("Use an HTTPS or git@host:path repository URL without credentials.")
			}
		}
	}
	if !ocrRef.MatchString(ref) {
		return fail("Use a branch, tag, or commit for --ref.")
	}
	return nil
}

var ocrSSHRepo = regexp.MustCompile(`^git@[A-Za-z0-9_.-]+:[A-Za-z0-9_./-]+$`)
var ocrRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
var ocrRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var ocrImageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ocrToken returns a fresh URL-safe random token; failures are propagated.
func ocrToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fail("Cannot generate OCR credentials.")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ocrHex returns a fresh unique release identifier; failures are propagated.
func ocrHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fail("Cannot create a unique OCR release directory.")
	}
	return strings.ToLower(hex.EncodeToString(b)), nil
}

// ocrStringArg reads a payload string the way Python `payload.get(key) or
// fallback` does: null or missing keys fall back, other non-strings error.
func ocrStringArg(payload Object, key string) (string, bool, error) {
	raw, ok := payload[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", false, fail("Invalid OCR request field: %s.", key)
	}
	return s, true, nil
}

// ManageOCR handles one OCR action against the cluster root. Install is an
// explicit action only; status never mutates anything and reports errors.
func ManageOCR(root string, payload Object) (Object, error) {
	action := str(value(payload, "action", ""))
	allowed := false
	for _, a := range ocrActions {
		if a == action {
			allowed = true
		}
	}
	if !allowed {
		return nil, fail("Choose OCR install, status, start, or stop.")
	}
	service, err := newOCRService(root)
	if err != nil {
		return nil, err
	}
	if action == "status" {
		return service.ocrStatus()
	}
	unlock, err := service.ocrLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	switch action {
	case "install":
		return service.ocrInstall(payload)
	case "start":
		result, err := service.ocrStart("", "")
		if err != nil {
			return nil, err
		}
		result["action"] = action
		return result, nil
	default:
		result, err := service.ocrStop()
		if err != nil {
			return nil, err
		}
		result["action"] = action
		return result, nil
	}
}

// ocrInstall performs the full explicit installation: validated repository,
// staged unique release, private log, image build or reuse, engine bootstrap,
// metadata write, start, and registration with correct rollback on failure.
// It never removes existing sources, credentials, or the model cache.
func (s *ocrService) ocrInstall(payload Object) (Object, error) {
	repo, hasRepo, err := ocrStringArg(payload, "repo")
	if err != nil {
		return nil, err
	}
	ref, _, err := ocrStringArg(payload, "ref")
	if err != nil {
		return nil, err
	}
	rebuild, hasRebuild, err := ocrBoolArg(payload, "rebuild")
	if err != nil {
		return nil, err
	}
	previous, err := s.ocrRecord()
	if err != nil {
		return nil, err
	}
	if !hasRepo || repo == "" {
		repo = str(previous["repo"])
	}
	if !hasRepo || ref == "" {
		if r, ok := previous["ref"]; ok && str(r) != "" {
			ref = str(r)
		} else {
			ref = "main"
		}
	}
	if err := ocrValidateRepository(repo, ref); err != nil {
		return nil, err
	}
	before, err := s.ocrStatus()
	if err != nil {
		return nil, err
	}
	oldSource, err := s.ocrSource()
	if err != nil {
		return nil, err
	}
	project, err := s.ocrProject()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(s.home, "releases"), 0o700); err != nil {
		return nil, fail("Cannot create OCR directories.")
	}
	nonce, err := ocrHex(8)
	if err != nil {
		return nil, err
	}
	stage := filepath.Join(s.home, "releases", nonce)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return nil, fail("Cannot create OCR directories.")
	}
	source := filepath.Join(stage, "source")
	logPath := filepath.Join(stage, "install.log")
	s.log = logPath
	logFile, err := ocrOpenLog(logPath)
	if err != nil {
		return nil, fail("Cannot create the private OCR installation log.")
	}
	defer logFile.Close()
	if err := ocrLogLine(logFile, "OCR installation started repo="+repo+" ref="+ref); err != nil {
		return nil, fail("Cannot write the private OCR installation log.")
	}
	// Streamed commands (clone, builds, bootstrap, registration) write their
	// full output into the private log; nothing is buffered in memory and
	// nothing raw reaches the public error.
	runStream := func(args []string, timeout time.Duration) error {
		if err := ocrLogLine(logFile, "$ "+strings.Join(args, " ")); err != nil {
			return fail("Cannot write the private OCR installation log.")
		}
		if err := ocrStreamRun(args, logFile, timeout); err != nil {
			_ = ocrLogLine(logFile, "command failed: "+err.Error())
			return fail("The OCR %s command failed. Check the private installation log: %s", args[0], logPath)
		}
		return nil
	}
	// Inspection and revision commands have small, bounded output and are
	// captured so they can be validated.
	runCapture := func(args []string, timeout time.Duration) (string, error) {
		result, err := ocrRun(args, nil, timeout)
		if err != nil || result.Code != 0 {
			_ = ocrLogLine(logFile, "command failed: "+filepath.Base(args[0]))
			return "", fail("The OCR %s command failed. Check the private installation log: %s", filepath.Base(args[0]), logPath)
		}
		return result.Out, nil
	}
	if err := runStream([]string{"git", "clone", "--quiet", "--no-checkout", "--", repo, source}, 300*time.Second); err != nil {
		return nil, err
	}
	if err := runStream([]string{"git", "-C", source, "checkout", "--quiet", "--detach", ref}, 300*time.Second); err != nil {
		return nil, err
	}
	revisionOut, err := runCapture([]string{"git", "-C", source, "rev-parse", "HEAD"}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	revision := strings.TrimSpace(revisionOut)
	if !ocrRevision.MatchString(revision) {
		return nil, fail("OCR git command failed. Check the private installation log: %s", logPath)
	}
	specName, specVersion, err := ocrReadSpec(filepath.Join(source, "pyproject.toml"))
	if err != nil {
		return nil, err
	}
	if specName != "spark-ocr" || !ocrSemanticVersion.MatchString(specVersion) {
		return nil, fail("The repository is not a supported spark-ocr release.")
	}
	if !ocrFileExists(filepath.Join(source, "scripts", "ensure_bifrost.py")) {
		return nil, fail("Update the OCR repository revision. It lacks the Sparky registration script.")
	}
	stable := filepath.Join(s.home, "service.env")
	if !ocrFileExists(stable) {
		old := s.ocrEnvFile(oldSource)
		if ocrFileExists(old) {
			content, err := os.ReadFile(old)
			if err != nil {
				return nil, fail("Cannot read the OCR environment.")
			}
			if err := ocrWriteFile(stable, string(content)); err != nil {
				return nil, err
			}
		} else {
			gateway, _, err := ocrStringArg(payload, "gateway")
			if err != nil {
				return nil, err
			}
			if err := ocrValidateGateway(gateway); err != nil {
				return nil, err
			}
			token, err := ocrToken()
			if err != nil {
				return nil, err
			}
			models := filepath.Join(s.home, "models")
			content := "SPARK_OCR_TOKEN=" + token + "\nOCR_BIFROST_URL=" + strings.TrimSuffix(gateway, "/") + "\nOCR_MODELS_HOST_DIR=" + models + "\n"
			if err := ocrWriteFile(stable, content); err != nil {
				return nil, err
			}
		}
	}
	// Preserve a custom model cache from the existing private environment.
	settings, err := readEnv(stable)
	if err != nil {
		return nil, fail("Cannot read the OCR environment.")
	}
	models := settings["OCR_MODELS_HOST_DIR"]
	if models == "" || !filepath.IsAbs(models) {
		return nil, fail("OCR_MODELS_HOST_DIR must be absolute.")
	}
	if err := os.MkdirAll(models, 0o700); err != nil {
		return nil, fail("Cannot create OCR directories.")
	}
	version := specVersion
	image := "spark-ocr:" + version
	indra := "spark-ocr-indra:" + version
	// Ask Docker only for the minimal image ID. A transport or permission
	// failure must never be mistaken for a missing image and trigger a
	// rebuild; only a confirmed "no such image" may choose to build.
	inspectOut, inspectErr := ocrRun([]string{"sudo", "-n", "docker", "image", "inspect", "--format", "{{.Id}}", image}, nil, 30*time.Second)
	cached := false
	switch {
	case inspectErr != nil:
		return nil, fail("Cannot inspect the OCR image. Check Docker and administrator access on Spark01.")
	case inspectOut.Code != 0:
		if !strings.Contains(strings.ToLower(inspectOut.Err), "no such image") {
			return nil, fail("Cannot inspect the OCR image. Check Docker and administrator access on Spark01.")
		}
	case ocrImageID.MatchString(strings.TrimSpace(inspectOut.Out)):
		cached = true
	default:
		return nil, fail("Cannot inspect the OCR image. Check Docker and administrator access on Spark01.")
	}
	if (hasRebuild && rebuild) || !cached {
		if err := runStream([]string{"sudo", "-n", "docker", "build", "-f", filepath.Join(source, "deploy", "Dockerfile.indra"), "-t", indra, source}, 7200*time.Second); err != nil {
			return nil, err
		}
		if err := runStream([]string{"sudo", "-n", "docker", "build", "-f", filepath.Join(source, "deploy", "Dockerfile"), "-t", image, "--build-arg", "INDRA_IMAGE=" + indra, source}, 7200*time.Second); err != nil {
			return nil, err
		}
	}
	// The spark-ocr engine bootstrap is engine-owned; it may run in production.
	if err := runStream([]string{"sudo", "-n", "docker", "run", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/tmp", "-e", "HF_HUB_DISABLE_XET=1", "-v", models + ":/models", image, "bootstrap"}, 1800*time.Second); err != nil {
		return nil, err
	}
	// Source is staged separately; nothing existing is overwritten.
	record := Object{"repo": repo, "ref": ref, "revision": revision, "source": source, "project": project}
	// Distinguish an absent metadata file from one that cannot be read: an
	// unreadable existing file must never be unlinked during rollback.
	saved, savedErr := os.ReadFile(s.metadata)
	if savedErr != nil && !os.IsNotExist(savedErr) {
		return nil, fail("Cannot read the OCR installation metadata.")
	}
	rollback := func() []string {
		var problems []string
		if savedErr == nil {
			if err := ocrWriteFile(s.metadata, string(saved)); err != nil {
				problems = append(problems, "metadata restore failed")
			}
		} else if err := os.Remove(s.metadata); err != nil && !os.IsNotExist(err) {
			problems = append(problems, "metadata cleanup failed")
		}
		if ocrIsTrue(before["running"]) {
			if _, err := s.ocrStart(oldSource, project); err != nil {
				problems = append(problems, "old service restart failed")
			}
		} else if _, err := s.ocrStop(); err != nil {
			problems = append(problems, "staged service stop failed")
		}
		return problems
	}
	recoverFailed := func(problems []string, cause error) (Object, error) {
		_ = ocrLogLine(logFile, "recovery needs attention: "+strings.Join(problems, "; "))
		_ = ocrLogLine(logFile, "installation failed: "+cause.Error())
		return nil, fail("OCR installation failed and recovery needs attention. Check the private installation log: %s", logPath)
	}
	activate := func() error {
		if err := ocrWriteFile(s.metadata, ocrMarshalRecord(record)); err != nil {
			return err
		}
		if _, err := s.ocrStart(source, project); err != nil {
			return err
		}
		// The engine's registration script is engine-owned; it may run in production.
		return runStream([]string{"python3", filepath.Join(source, "scripts", "ensure_bifrost.py"),
			"--gateway-root", filepath.Join(s.root, "gateway"), "--service-env", stable,
			"--provider-template", filepath.Join(source, "config", "bifrost.example.json")}, 180*time.Second)
	}
	if err := activate(); err != nil {
		// Roll back the metadata change and restore the previous service
		// state without touching existing sources, credentials, or caches.
		if problems := rollback(); len(problems) > 0 {
			return recoverFailed(problems, err)
		}
		return nil, err
	}
	result, err := s.ocrStatus()
	if err != nil {
		if problems := rollback(); len(problems) > 0 {
			return recoverFailed(problems, err)
		}
		return nil, err
	}
	if !ocrIsTrue(result["running"]) || !ocrIsTrue(result["api_ready"]) {
		ready := fail("OCR did not become ready after installation.")
		if problems := rollback(); len(problems) > 0 {
			return recoverFailed(problems, ready)
		}
		return nil, ready
	}
	result["action"] = "install"
	result["image_reused"] = cached && !(hasRebuild && rebuild)
	result["bifrost_registered"] = true
	result["log"] = logPath
	return result, nil
}

// ocrBoolArg reads an optional boolean the way Python `payload.get` does:
// missing or null means false; anything else must be a real boolean.
func ocrBoolArg(payload Object, key string) (bool, bool, error) {
	raw, ok := payload[key]
	if !ok || raw == nil {
		return false, false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, false, fail("Invalid OCR request field: %s.", key)
	}
	return b, true, nil
}

// ocrReadSpec parses pyproject.toml; parse errors are never ignored.
func ocrReadSpec(path string) (name string, version string, err error) {
	var spec struct {
		Project struct {
			Name    string `toml:"name"`
			Version string `toml:"version"`
		} `toml:"project"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fail("The repository is not a supported spark-ocr release.")
	}
	if err := toml.Unmarshal(b, &spec); err != nil {
		return "", "", fail("The repository is not a supported spark-ocr release.")
	}
	return spec.Project.Name, spec.Project.Version, nil
}

func ocrMarshalRecord(record Object) string {
	b, _ := json.MarshalIndent(record, "", "  ")
	return string(b) + "\n"
}

var ocrSemanticVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// ocrValidateGateway requires a bare HTTPS origin; ports and IPv6 hosts are
// allowed, but userinfo, query, fragment, controls, and non-origin paths are
// rejected.
func ocrValidateGateway(gateway string) error {
	if gateway == "" || strings.ContainsFunc(gateway, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fail("Set an HTTPS gateway origin before installing OCR.")
	}
	parsed, err := url.Parse(gateway)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return fail("Set an HTTPS gateway origin before installing OCR.")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || (len(port) > 1 && port[0] == '0') {
			return fail("Set an HTTPS gateway origin before installing OCR.")
		}
	}
	return nil
}
