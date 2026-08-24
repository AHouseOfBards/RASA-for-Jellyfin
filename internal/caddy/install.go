package caddy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/service"
)

// BinaryName is the bundled executable's filename.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "caddy.exe"
	}
	return "caddy"
}

// ErrBinaryNotFound means no Caddy was bundled with this build and none is on
// PATH. It is a packaging failure rather than anything the user did.
var ErrBinaryNotFound = errors.New("no caddy binary was found")

// Installer writes the configuration and registers Caddy as a service.
//
// The split matters: Generate produces text and is trivially testable, while
// this touches the filesystem and the service manager. Everything here is
// written to be safe to re-run, because SPEC.md §10 requires every transition
// to be idempotent and this one sits in the middle of the longest unattended
// stretch of the install.
type Installer struct {
	// BinaryPath is the Caddy executable to install. Required.
	BinaryPath string
	// CaddyfilePath is where the generated configuration is written.
	CaddyfilePath string
	// DataDir becomes XDG_DATA_HOME, so certificates and the ACME account key
	// land somewhere durable instead of the service account's profile.
	DataDir string
	// EnvFile, when set, receives the service's environment instead of the
	// service definition carrying it.
	//
	// This exists for systemd, whose unit files are world-readable: a Dynu
	// token passed through Environment= would be readable by every account on
	// the machine. Windows keeps service environment in the registry under the
	// service key, which is already administrator-only, so this stays empty
	// there.
	EnvFile string
	// Services registers the service. Required.
	Services service.Manager
	Log      *logging.Logger
}

// FindBinary locates the bundled Caddy.
//
// Bundled first, PATH second. A developer machine with Caddy installed is the
// only case where PATH wins, and it is worth supporting because it makes the
// install path runnable without building a release bundle — but a stock Caddy
// lacks the Dynu module and fails at start with a message that does not
// obviously mean "wrong binary", which is why Validate looks for exactly that.
func FindBinary(dirs ...string) (string, error) {
	name := BinaryName()
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", ErrBinaryNotFound
}

// Install writes the Caddyfile, validates it against the real binary, and
// registers and starts the service.
//
// Validation before installation is the point of the ordering. A Caddyfile
// that a stock binary cannot parse — the missing-Dynu-module case — otherwise
// fails inside a service that has already reported "started", where the only
// evidence is a log the user has not been told about yet.
func (in *Installer) Install(ctx context.Context, cfg Config, env map[string]string) error {
	if in.BinaryPath == "" {
		return ErrBinaryNotFound
	}
	if in.Services == nil {
		return errors.New("caddy installer has no service manager")
	}

	text, err := cfg.Generate()
	if err != nil {
		return fmt.Errorf("generating the proxy configuration: %w", err)
	}
	if err := writeFile(in.CaddyfilePath, text); err != nil {
		return fmt.Errorf("writing %s: %w", in.CaddyfilePath, err)
	}
	in.log("wrote the proxy configuration", slog.String("path", in.CaddyfilePath))

	if err := in.Validate(ctx, env); err != nil {
		return err
	}

	serviceEnv := map[string]string{}
	for k, v := range env {
		serviceEnv[k] = v
	}
	if in.DataDir != "" {
		// Caddy stores certificates under XDG_DATA_HOME/caddy. Left to the
		// default, a Windows service would put them in the service account's
		// profile, where a later run under a different account re-issues from
		// scratch and spends Let's Encrypt quota for no reason.
		serviceEnv["XDG_DATA_HOME"] = in.DataDir
		serviceEnv["XDG_CONFIG_HOME"] = in.DataDir
	}

	def := service.CaddyDefinition(in.BinaryPath, in.CaddyfilePath, filepath.Dir(in.CaddyfilePath), serviceEnv)
	if in.EnvFile != "" {
		if err := WriteEnvFile(in.EnvFile, serviceEnv); err != nil {
			return fmt.Errorf("writing the service environment: %w", err)
		}
		def.Environment = nil
		def.EnvironmentFile = in.EnvFile
	}
	if err := in.Services.InstallService(ctx, def); err != nil {
		return fmt.Errorf("registering the proxy service: %w", err)
	}
	in.log("registered the proxy service", slog.String("name", def.Name))

	// A re-run reaches an already-running service holding the old file. Stop
	// and start rather than start alone, so a repair actually applies the
	// configuration it just wrote.
	if err := in.Services.StopService(ctx, def.Name); err != nil {
		in.log("could not stop the existing proxy service", slog.Any("err", err))
	}
	if err := in.Services.StartService(ctx, def.Name); err != nil {
		return fmt.Errorf("starting the proxy service: %w", err)
	}
	in.log("started the proxy service", slog.String("name", def.Name))
	return nil
}

// Validate runs "caddy validate" against the generated file.
func (in *Installer) Validate(ctx context.Context, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, in.BinaryPath, "validate", "--config", in.CaddyfilePath, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(), envPairs(env)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		in.log("proxy configuration validated")
		return nil
	}

	detail := strings.TrimSpace(string(out))
	if isMissingModule(detail) {
		// This is the packaging failure the package doc warns about, and it
		// is worth naming precisely: a stock Caddy parses everything else in
		// the file and fails only on the module it does not have.
		return fmt.Errorf("the bundled proxy is missing the DNS module it needs (built without caddy-dns/dynu): %s", firstLine(detail))
	}
	return fmt.Errorf("the proxy configuration was rejected: %s", firstLine(detail))
}

// Version reports the installed Caddy's version string, for the state file.
func (in *Installer) Version(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, in.BinaryPath, "version").Output()
	if err != nil {
		return ""
	}
	return firstLine(strings.TrimSpace(string(out)))
}

// Stage copies a bundled binary into dstDir, which is where it must live to
// outlive RASA's own install directory.
//
// Copying rather than pointing the service at the install directory is the
// whole reason the uninstaller can remove RASA without taking the proxy with
// it (SPEC.md §3).
func Stage(src, dstDir string) (string, error) {
	dst := filepath.Join(dstDir, BinaryName())
	if sameFile(src, dst) {
		return dst, nil
	}
	source, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer source.Close()

	// A running service holds the destination open on Windows, so write beside
	// it and rename — the same reason the state store does.
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func writeFile(path, text string) error {
	if path == "" {
		return errors.New("no path for the proxy configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// isMissingModule recognises the specific failure a stock Caddy produces
// against a file using the Dynu module.
func isMissingModule(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "unrecognized directive") ||
		strings.Contains(l, "unknown directive") ||
		strings.Contains(l, "getting module named") ||
		(strings.Contains(l, "module") && strings.Contains(l, "dns.providers.dynu"))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func (in *Installer) log(msg string, args ...any) {
	if in.Log != nil {
		in.Log.Info(msg, args...)
	}
}

// WriteEnvFile writes a KEY=value file readable only by its owner.
//
// The format is what systemd's EnvironmentFile expects: one assignment per
// line, no quoting, no export. Values containing newlines are refused rather
// than silently truncating the file at the first one, which would leave a
// service running with a partial credential and no indication why.
func WriteEnvFile(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# Generated by RASA for Jellyfin. Do not edit by hand.\n")
	for _, k := range keys {
		v := env[k]
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("value for %s contains a line break", k)
		}
		b.WriteString(k + "=" + v + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written directly rather than through a temporary file: a rename would
	// carry the temporary file's permissions, and a credential briefly at 0644
	// is a credential leaked.
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
