package crq

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type PreflightOptions struct {
	Binary     string
	ReviewType string
	Base       string
	BaseCommit string
	Dir        string
	Light      bool
	Timeout    time.Duration
	ExtraArgs  []string
}

type PreflightReport struct {
	Status        string             `json:"status"`
	Tool          string             `json:"tool"`
	Command       []string           `json:"command"`
	ReviewContext map[string]any     `json:"review_context,omitempty"`
	Statuses      []PreflightStatus  `json:"statuses"`
	Complete      map[string]any     `json:"complete,omitempty"`
	Findings      []PreflightFinding `json:"findings"`
	Stderr        string             `json:"stderr,omitempty"`
	Error         string             `json:"error,omitempty"`
	ExitCode      int                `json:"exit_code"`
	CheckedAt     time.Time          `json:"checked_at"`
	DurationMS    int64              `json:"duration_ms"`
}

type PreflightStatus struct {
	Phase   string `json:"phase,omitempty"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

type PreflightFinding struct {
	ID                  string   `json:"id"`
	Bot                 string   `json:"bot"`
	Severity            string   `json:"severity"`
	Path                string   `json:"path,omitempty"`
	Line                int      `json:"line,omitempty"`
	EndLine             int      `json:"end_line,omitempty"`
	Title               string   `json:"title"`
	Body                string   `json:"body"`
	CodegenInstructions string   `json:"codegen_instructions,omitempty"`
	Suggestions         []string `json:"suggestions,omitempty"`
	Fingerprint         string   `json:"fingerprint,omitempty"`
	Source              string   `json:"source"`
}

func Preflight(ctx context.Context, opts PreflightOptions) (PreflightReport, int, error) {
	start := time.Now()
	report := PreflightReport{
		Status:    "preflight",
		Statuses:  []PreflightStatus{},
		Findings:  []PreflightFinding{},
		CheckedAt: start.UTC(),
	}
	binary, err := coderabbitBinary(opts.Binary)
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
		report.ExitCode = 1
		report.DurationMS = time.Since(start).Milliseconds()
		return report, 1, err
	}
	args := coderabbitArgs(opts)
	report.Tool = binary
	report.Command = append([]string{binary}, args...)

	runCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
		report.ExitCode = 1
		report.DurationMS = time.Since(start).Milliseconds()
		return report, 1, err
	}
	if err := cmd.Start(); err != nil {
		report.Status = "error"
		report.Error = err.Error()
		report.ExitCode = 1
		report.DurationMS = time.Since(start).Milliseconds()
		return report, 1, err
	}
	parseErr := parsePreflightStream(stdout, &report)
	waitErr := cmd.Wait()
	report.Stderr = trimForReport(stderr.String(), 4000)
	report.DurationMS = time.Since(start).Milliseconds()
	if runCtx.Err() != nil {
		report.Status = "error"
		report.Error = runCtx.Err().Error()
		report.ExitCode = 2
		return report, 2, runCtx.Err()
	}
	if parseErr != nil {
		report.Status = "error"
		report.Error = parseErr.Error()
		report.ExitCode = 1
		return report, 1, parseErr
	}
	if waitErr != nil {
		code := 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		report.Status = "error"
		report.Error = waitErr.Error()
		report.ExitCode = code
		return report, code, waitErr
	}
	if len(report.Findings) == 0 {
		report.Status = "clean"
		report.ExitCode = 0
		return report, 0, nil
	}
	report.Status = "feedback"
	report.ExitCode = 10
	return report, 10, nil
}

func coderabbitBinary(explicit string) (string, error) {
	candidates := []string{explicit, os.Getenv("CRQ_CODERABBIT_BIN"), "cr", "coderabbit"}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
	}
	return "", errors.New("CodeRabbit CLI not found (install cr/coderabbit or set CRQ_CODERABBIT_BIN)")
}

func coderabbitArgs(opts PreflightOptions) []string {
	args := []string{"review", "--agent"}
	if opts.Light {
		args = append(args, "--light")
	}
	if opts.ReviewType != "" {
		args = append(args, "--type", opts.ReviewType)
	}
	if opts.Base != "" {
		args = append(args, "--base", opts.Base)
	}
	if opts.BaseCommit != "" {
		args = append(args, "--base-commit", opts.BaseCommit)
	}
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func parsePreflightStream(r io.Reader, report *PreflightReport) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return fmt.Errorf("failed to parse CodeRabbit --agent JSON line: %w", err)
		}
		applyPreflightEvent(report, event)
	}
	return scanner.Err()
}

func applyPreflightEvent(report *PreflightReport, event map[string]any) {
	switch stringField(event, "type") {
	case "review_context":
		report.ReviewContext = event
	case "status", "heartbeat":
		report.Statuses = append(report.Statuses, PreflightStatus{
			Phase:   stringField(event, "phase"),
			Status:  stringField(event, "status"),
			Message: stringField(event, "message"),
		})
	case "complete":
		report.Complete = event
	case "finding":
		if finding := preflightFinding(event); finding.Title != "" || finding.Body != "" || finding.Path != "" {
			report.Findings = append(report.Findings, finding)
		}
	case "error":
		if msg := firstNonEmpty(stringField(event, "message"), stringField(event, "error")); msg != "" {
			report.Error = msg
		}
	}
}

func preflightFinding(event map[string]any) PreflightFinding {
	codegen := stringField(event, "codegenInstructions")
	comment := stringField(event, "comment")
	body := firstNonEmpty(codegen, comment)
	title := firstNonEmpty(stringField(event, "title"), titleOf(body))
	severity := strings.ToLower(firstNonEmpty(stringField(event, "severity"), severityOf(title+"\n"+body)))
	finding := PreflightFinding{
		ID:                  firstNonEmpty(stringField(event, "id"), stringField(event, "fingerprint")),
		Bot:                 "coderabbit-cli",
		Severity:            severity,
		Path:                firstNonEmpty(stringField(event, "fileName"), stringField(event, "path")),
		Line:                intField(event, "startLine", "line"),
		EndLine:             intField(event, "endLine"),
		Title:               title,
		Body:                body,
		CodegenInstructions: codegen,
		Suggestions:         stringSliceField(event, "suggestions"),
		Fingerprint:         stringField(event, "fingerprint"),
		Source:              "coderabbit_cli",
	}
	if finding.ID == "" {
		sum := sha1.Sum([]byte(finding.Path + "|" + strconv.Itoa(finding.Line) + "|" + finding.Title + "|" + finding.Body))
		finding.ID = hex.EncodeToString(sum[:])
	}
	return finding
}

func stringField(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func intField(value map[string]any, keys ...string) int {
	for _, key := range keys {
		raw, ok := value[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}

func stringSliceField(value map[string]any, key string) []string {
	raw, ok := value[key]
	if !ok || raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		if s := stringField(value, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func trimForReport(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "...(truncated)"
}
