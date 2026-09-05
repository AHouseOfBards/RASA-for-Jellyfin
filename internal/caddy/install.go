package caddy

import (
	"context"
	"errors"
	"fmt"
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

	// The host firewall comes last, and its failure is not fatal. A machine
	// with no firewall enabled reaches here having already succeeded, and a
	// machine where the rule could not be written still works for anyone whose
	// traffic the firewall does not stop - reporting a failed setup would be
	// wrong in both cases.
	if err := service.AllowProgram(ctx, in.BinaryPath, in.Log); err != nil {
		in.log("could not write a firewall rule", slog.Any("err", err))
	}
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

	// A binary that could not be run at all is not a verdict on the file.
	// Without this the message reads "the proxy configuration was rejected:"
	// followed by nothing, which sends whoever reads it to inspect a
	// Caddyfile that was never looked at.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("could not run the bundled proxy at %s: %w", in.BinaryPath, err)
	}

	detail := errorLine(string(out))
	if detail == "" {
		// Caddy redirects its own logger into the configured log file, so a
		// failure can leave nothing on stdout to quote.
		return fmt.Errorf("the bundled proxy rejected the configuration and said nothing; %s may explain why", in.CaddyfilePath)
	}
	if directive, module, ok := missingModule(detail); ok {
		// This is the packaging failure the package doc warns about, and it is
		// worth naming precisely: Caddy parses every other line in the file and
		// fails only on the directive whose module is absent, so the message
		// has to say which one — "built without a module" sends whoever reads
		// it to check the wrong thing.
		return fmt.Errorf("the bundled proxy is missing the %s module, which provides the %q directive (build it with packaging/caddy/build.sh): %s",
			module, directive, detail)
	}
	return fmt.Errorf("the proxy configuration was rejected: %s", detail)
}

// errorLine picks the line that says what went wrong.
//
// Caddy writes an informational JSON line to stderr before it reports
// anything, so the first line of output is reliably "using config from file"
// and reliably useless. This was caught by running a real Caddy against a real
// generated file, which no amount of string comparison would have found.
func errorLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "{") {
			// Skip blanks and Caddy's structured log lines.
			continue
		}
		return line
	}
	return strings.TrimSpace(out)
}

// modulesByDirective maps the directives the generated file uses to the module
// that has to be compiled in for them to exist.
var modulesByDirective = map[string]string{
	"rate_limit": "caddy-ratelimit",
	"dynu":       "caddy-dns/dynu",
}

// missingModule identifies which directive Caddy did not recognise, and which
// module supplies it.
func missingModule(detail string) (directive, module string, ok bool) {
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "dns.providers.dynu") {
		return "dns dynu", "caddy-dns/dynu", true
	}
	marker := "unrecognized directive: "
	i := strings.Index(lower, marker)
	if i < 0 {
		marker = "unknown directive: "
		i = strings.Index(lower, marker)
	}
	if i < 0 {
		return "", "", false
	}
	directive = strings.Fields(detail[i+len(marker):])[0]
	if module, known := modulesByDirective[directive]; known {
		return directive, module, true
	}
	// An unrecognised directive RASA does not know about still means a wrong
	// binary; naming the directive is more useful than guessing the module.
	return directive, "required", true
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

// Stage copies the bundled Caddy somewhere it will survive RASA's removal.
func Stage(src, dstDir string) (string, error) {
	return service.StageBinary(src, dstDir, BinaryName())
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

// RequiredModules are the two the generated configuration cannot work without.
var RequiredModules = []string{"dns.providers.dynu", "http.handlers.rate_limit"}

// MissingModules reports which required modules a Caddy binary lacks.
//
// Worth asking early. FindBinary falls back to whatever "caddy" is on PATH,
// which on a machine that already runs Caddy for something else is a stock
// build with neither of these in it. Nothing then goes wrong until the
// configuration is validated, minutes later, after a hostname has been claimed
// and a DNS record has propagated -- so the same question is asked at startup,
// where the answer costs nothing and the user has lost nothing.
//
// An error means the question could not be asked at all, which is not the same
// as the modules being absent and must not be reported as if it were.
func MissingModules(ctx context.Context, binary string) ([]string, error) {
	out, err := exec.CommandContext(ctx, binary, "list-modules").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("asking %s which modules it has: %w", binary, err)
	}
	var missing []string
	for _, id := range RequiredModules {
		if !strings.Contains(string(out), id) {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
