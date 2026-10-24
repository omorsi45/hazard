package mine

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Caller facts must describe one complete terminal trace and one decoded,
// unambiguous job-scoped JUnit body. Source/pipeline SHA is not tested HEAD.
type gitlabProvenanceContext struct {
	ProjectID, PipelineID, JobID, JobProjectID, JobPipelineID, TraceJobID int64
	JobStatus, FailureReason, ReportSHA256                                string
	TraceComplete, ReportValidated                                        bool
}

type gitlabProvenance struct {
	TestedSHA, LogSHA256, Language, ReportPath, ProducerEvidence string
}

type gitlabProvenanceError struct{ Reason string }

func (e *gitlabProvenanceError) Error() string { return "gitlab provenance: " + e.Reason }

var (
	gitLabSection = regexp.MustCompile(`^(?:\x1b\[0K)?section_(start|end):([0-9]+):([a-z_]+)\r\x1b\[0K(.*)$`)
	gitLabCount   = regexp.MustCompile("^([^:]+): +found +([0-9]+) +matching artifact files and directories$")
)

// shortcut: untimestamped traditional POSIX pytest traces only, expand after source-backed grammar approval.
// This corroborates recorded execution under the caller's honest-source
// policy; log-controlled markers cannot attest freshness or byte continuity.
func verifyGitLabProvenance(trace []byte, c gitlabProvenanceContext) (gitlabProvenance, error) {
	reject := func(reason string) (gitlabProvenance, error) {
		return gitlabProvenance{}, &gitlabProvenanceError{Reason: reason}
	}
	if c.ProjectID <= 0 || c.PipelineID <= 0 || c.JobID <= 0 ||
		c.JobProjectID != c.ProjectID || c.JobPipelineID != c.PipelineID || c.TraceJobID != c.JobID {
		return reject("context")
	}
	if !c.TraceComplete || !c.ReportValidated || !gitLabLowerHex(c.ReportSHA256, 64) {
		return reject("acquisition")
	}
	uploadStage, terminal := "upload_artifacts_on_success", "Job succeeded"
	if c.JobStatus == "failed" && c.FailureReason == "script_failure" {
		uploadStage, terminal = "upload_artifacts_on_failure", "Job failed: exit code 1"
	} else if c.JobStatus != "success" || c.FailureReason != "" {
		return reject("job_status")
	}
	if len(trace) == 0 || len(trace) > 16*1024*1024 || !utf8.Valid(trace) || trace[len(trace)-1] != '\n' {
		return reject("trace_incomplete")
	}
	stage, phase, command, upload := "", 0, 0, 0
	sha, path := "", ""
	seen := make(map[string]bool)
	runtimeSeen, headerSeen, terminalSeen := false, false, false
	for remaining := string(trace); remaining != ""; {
		raw, rest, _ := strings.Cut(remaining, "\n")
		remaining = rest
		if len(raw) > 64*1024 || terminalSeen {
			return reject("trace_framing")
		}
		// Parse carriage-return marker syntax before any ANSI normalization.
		marker := gitLabSection.FindStringSubmatch(raw)
		content := raw
		if marker != nil {
			if _, err := strconv.ParseUint(marker[2], 10, 64); err != nil {
				return reject("trace_framing")
			}
			content = marker[4]
		}
		line, ok := gitLabPlainLine(content)
		if !ok {
			return reject("trace_framing")
		}
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if strings.Contains(lower, "truncat") || strings.Contains(lower, "exceeded limit") ||
			strings.Contains(lower, "log limit") || strings.Contains(lower, "output limit") ||
			strings.Contains(lower, "trace data lost") || strings.Contains(lower, "log output lost") ||
			strings.Contains(lower, "log output discarded") {
			return reject("trace_incomplete")
		}
		if strings.Contains(line, "section_start:") || strings.Contains(line, "section_end:") ||
			strings.Contains(lower, "collapsed") {
			return reject("trace_framing")
		}
		if !headerSeen {
			if marker != nil || !strings.HasPrefix(line, "Running with gitlab-runner ") ||
				len(line) == len("Running with gitlab-runner ") {
				return reject("runner_header")
			}
			headerSeen = true
			continue
		}
		if marker != nil {
			// Human headings are not evidence and may not hide another operation.
			if strings.Contains(line, "$") || strings.Contains(strings.Join(strings.Fields(line), " "), ": found ") ||
				strings.Contains(line, "Uploading artifacts...") || strings.Contains(line, "Uploading artifacts as ") {
				return reject("trace_framing")
			}
			name := marker[3]
			if marker[1] == "start" {
				if stage != "" || seen[name] {
					return reject("stage_order")
				}
				switch name {
				case "step_script":
					if phase != 0 {
						return reject("stage_order")
					}
				case uploadStage:
					if phase != 1 {
						return reject("stage_order")
					}
				case "cleanup_file_variables":
					if phase != 2 {
						return reject("stage_order")
					}
				case "resolve_secrets", "prepare_executor", "prepare_script", "get_sources", "restore_cache", "download_artifacts":
					if phase != 0 {
						return reject("stage_order")
					}
				default:
					return reject("unsupported_stage")
				}
				stage, seen[name] = name, true
			} else {
				if stage != name {
					return reject("stage_order")
				}
				switch stage {
				case "step_script":
					if command != 3 {
						return reject("commands")
					}
					phase = 1
				case uploadStage:
					if upload != 3 {
						return reject("upload")
					}
					phase = 2
				case "cleanup_file_variables":
					phase = 3
				}
				stage = ""
			}
			continue
		}
		if line == "" {
			continue
		}
		if stage == "step_script" {
			switch command {
			case 0:
				if line != "$ git rev-parse HEAD" {
					return reject("commands")
				}
				command = 1
			case 1:
				if !gitLabLowerHex(line, 40) {
					return reject("head")
				}
				sha, command = line, 2
			case 2:
				const prefix = "$ pytest --junitxml="
				if !strings.HasPrefix(line, prefix) || !gitLabReportPath(strings.TrimPrefix(line, prefix)) {
					return reject("commands")
				}
				path, command = strings.TrimPrefix(line, prefix), 3
			case 3:
				if strings.HasPrefix(line, "$") {
					return reject("commands")
				}
				// Subsequent stdout is opaque, never HEAD or uploader evidence.
			}
			continue
		}
		if strings.HasPrefix(line, "$") {
			return reject("commands")
		}
		if stage == uploadStage {
			if strings.HasPrefix(line, "Runtime platform ") {
				if runtimeSeen || upload > 1 || !gitLabRuntimeLine(line) {
					return reject("upload")
				}
				runtimeSeen = true
				continue
			}
			switch upload {
			case 0:
				if line != "Uploading artifacts..." {
					return reject("upload")
				}
				upload = 1
			case 1:
				count := gitLabCount.FindStringSubmatch(line)
				if count == nil || count[1] != path || count[2] != "1" {
					return reject("upload_path")
				}
				upload = 2
			case 2:
				if !gitLabUploadSucceeded(line, c.JobID) {
					return reject("upload_status")
				}
				upload = 3
			default:
				return reject("upload")
			}
			continue
		}
		if stage == "" && phase != 0 {
			final := line
			if c.JobStatus == "failed" {
				final = strings.TrimPrefix(line, "ERROR: ")
			}
			if phase != 3 || final != terminal {
				return reject("terminal")
			}
			terminalSeen = true
		}
	}
	if !terminalSeen || stage != "" || phase != 3 {
		return reject("trace_incomplete")
	}
	// ReportSHA256 records acquired bytes, not a digest proved by the uploader.
	// ReportPath is producer evidence, distinct from gzip's logical junit.xml.
	return gitlabProvenance{
		TestedSHA: sha, LogSHA256: fmt.Sprintf("%x", sha256.Sum256(trace)),
		Language: "python", ReportPath: path,
		ProducerEvidence: fmt.Sprintf("project=%d pipeline=%d job=%d report_sha256=%s method=gitlab-runner-pytest-junit-v1",
			c.ProjectID, c.PipelineID, c.JobID, c.ReportSHA256),
	}, nil
}

func gitLabLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, ch := range value {
		hexDigit := ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f'
		if !hexDigit {
			return false
		}
	}
	return true
}

func gitLabReportPath(path string) bool {
	if !strings.HasSuffix(path, ".xml") {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, ch := range component {
			pathChar := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
				ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-'
			if !pathChar {
				return false
			}
		}
	}
	return true
}

// Only Runner's color and edge clear-line escapes are supported.
func gitLabPlainLine(raw string) (string, bool) {
	var plain strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '\x1b' {
			plain.WriteByte(raw[i])
			i++
			continue
		}
		if strings.HasPrefix(raw[i:], "\x1b[0K") && (i == 0 || i+4 == len(raw)) {
			i += 4
			continue
		}
		if !strings.HasPrefix(raw[i:], "\x1b[") {
			return "", false
		}
		end := strings.IndexByte(raw[i+2:], 'm')
		if end < 0 || end > 20 {
			return "", false
		}
		for _, part := range strings.Split(raw[i+2:i+2+end], ";") {
			if part == "" {
				continue
			}
			for _, ch := range part {
				if ch < '0' || ch > '9' {
					return "", false
				}
			}
			n, err := strconv.Atoi(part)
			colorCode := n == 0 || n == 1 || n == 39 || n == 49 ||
				n >= 30 && n <= 37 || n >= 40 && n <= 47 || n >= 90 && n <= 97 || n >= 100 && n <= 107
			if err != nil || !colorCode {
				return "", false
			}
		}
		i += 3 + end
	}
	result := plain.String()
	for _, ch := range result {
		if unicode.IsControl(ch) && ch != '\t' {
			return "", false
		}
	}
	return result, true
}

func gitLabRuntimeLine(line string) bool {
	fields := strings.Fields(strings.TrimPrefix(line, "Runtime platform"))
	if len(fields) != 5 {
		return false
	}
	seen := make(map[string]bool, 5)
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" || seen[key] {
			return false
		}
		switch key {
		case "arch", "os", "version", "revision":
			for _, ch := range value {
				metadataChar := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
					ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-' || ch == '+'
				if !metadataChar {
					return false
				}
			}
		case "pid":
			if pid, err := strconv.ParseInt(value, 10, 64); err != nil || pid <= 0 {
				return false
			}
		default:
			return false
		}
		seen[key] = true
	}
	return true
}

func gitLabUploadSucceeded(line string, jobID int64) bool {
	const prefix = "Uploading artifacts as \"junit\" to coordinator..."
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	idSeen, statusSeen := false, false
	tail := strings.TrimPrefix(line, prefix)
	if strings.Contains(tail, "Uploading artifacts") || strings.Contains(strings.Join(strings.Fields(tail), " "), ": found ") {
		return false
	}
	for _, field := range strings.Fields(tail) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			lower := strings.ToLower(field)
			if strings.Contains(lower, "redirect") || strings.Contains(lower, "retry") ||
				strings.Contains(lower, "error") || strings.Contains(lower, "warn") {
				return false
			}
			continue
		}
		switch key {
		case "id":
			if idSeen || value != strconv.FormatInt(jobID, 10) {
				return false
			}
			idSeen = true
		case "status":
			if statusSeen || value != "201" {
				return false
			}
			statusSeen = true
		}
	}
	return idSeen && statusSeen
}
