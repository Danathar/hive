package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Live model discovery for Google's Antigravity CLI.
//
// `agy models` is the CLI's own account-aware model inventory. It prints one
// tab-delimited `<id>\t<display name>` record per model, after a progress line.
// Running the vendor command is preferable to copying a catalog indefinitely:
// availability is tied to the signed-in Google account and changes between CLI
// releases. Hive never reads or forwards Antigravity's OAuth material; the
// subprocess resolves it exactly as an agent does from HOME/.gemini.
//
// The main hive image installs agy, but a developer build may not. Missing
// binary, signed-out state, timeout, malformed output, or an empty inventory
// all degrade to agyStaticModels with fallback=true through queryCLIModels.

const (
	agyBackendID          = "agy"
	agyBinaryName         = "agy"
	agyModelsSubcommand   = "models"
	agyModelsProbeTimeout = 5 * time.Second
	agyAgentHome          = "/data/home"
)

// agyStaticModels is the pre-discovery floor used on first dashboard paint and
// whenever `agy models` cannot run. Snapshot from agy 1.1.18 on 2026-08-31,
// preserving the CLI's own order. Keep in sync with AGY_CLI_MODELS in
// static/index.html.
var agyStaticModels = []string{
	"gemini-3.7-flash-high",
	"gemini-3.7-flash-medium",
	"gemini-3.7-flash-low",
	"gemini-3.6-flash-high",
	"gemini-3.6-flash-medium",
	"gemini-3.6-flash-low",
	"gemini-3.1-pro-high",
	"gemini-3.1-pro-low",
	"claude-sonnet-4-6",
	"claude-opus-4-6-thinking",
	"gpt-oss-120b-medium",
}

// runAgyModelsProbe is a test seam. Production always uses
// execAgyModelsProbe; tests substitute a deterministic inventory or error.
var runAgyModelsProbe = execAgyModelsProbe

func (s *Server) discoverAgyModels() cliModelResult {
	models, err := runAgyModelsProbe()
	if err != nil || len(models) == 0 {
		if err != nil && s.logger != nil {
			// Do not include stdout/stderr: the CLI owns them and may mention
			// account or local-path details. The error names only the failed
			// operation/exit status.
			s.logger.Info("agy model discovery unavailable, serving static fallback", "err", err.Error())
		}
		return cliModelResult{fallback: true}
	}
	return cliModelResult{models: dedupeModels(models), fallback: false}
}

func execAgyModelsProbe() ([]string, error) {
	bin, err := exec.LookPath(agyBinaryName)
	if err != nil {
		return nil, fmt.Errorf("agy binary not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agyModelsProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, agyModelsSubcommand)

	// Per-UID interactive homes bridge .gemini back to the shared agent HOME,
	// so one probe under /data/home sees the same signed-in account without
	// guessing which agent owns it. Outside the image (where /data/home is
	// absent), preserve the developer's current HOME and working directory.
	if st, statErr := os.Stat(agyAgentHome); statErr == nil && st.IsDir() {
		cmd.Env = append(os.Environ(), "HOME="+agyAgentHome)
		cmd.Dir = agyAgentHome
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// Antigravity emits progress and language-server logs on stderr. They are
	// neither part of the model-list contract nor safe diagnostic material for
	// a server log, so discard them.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("agy models timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("agy models failed: %w", err)
	}
	return parseAgyModelsOutput(&stdout)
}

// parseAgyModelsOutput accepts only the observed tab-delimited records.
// Progress prose and future non-record chatter are ignored; an output with no
// valid records is an error so callers cannot mistake a changed format for an
// authoritative empty catalog and auto-heal agents off working models.
func parseAgyModelsOutput(r io.Reader) ([]string, error) {
	var models []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		id, _, ok := strings.Cut(line, "\t")
		id = strings.TrimSpace(id)
		if !ok || id == "" || strings.ContainsAny(id, " \r\n\t") {
			continue
		}
		models = append(models, id)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read agy models output: %w", err)
	}
	models = dedupeModels(models)
	if len(models) == 0 {
		return nil, errors.New("agy models returned no tab-delimited model records")
	}
	return models, nil
}
