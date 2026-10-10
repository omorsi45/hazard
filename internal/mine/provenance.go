package mine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type githubProvenanceContext struct {
	Repository                                                  string
	RunID, JobRunID, JobID, LogJobID, ArtifactID, ArtifactRunID int64
	Attempt, JobAttempt                                         int
	ArchiveSHA256, ArtifactDigest                               string
	ArchiveSize, ArtifactSize                                   int64
	LogComplete                                                 bool
}

type githubProvenance struct {
	TestedSHA, CheckoutRef, LogSHA256, ProducerEvidence, Language, ReportPath string
}

type githubProvenanceError struct{ Reason string }

func (e *githubProvenanceError) Error() string { return "GitHub provenance: " + e.Reason }

const maxGitHubLogBytes = 16 << 20

var (
	logANSI           = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	checkoutAction    = regexp.MustCompile(`^actions/checkout@[0-9a-f]{40}$`)
	uploadAction      = regexp.MustCompile(`^actions/upload-artifact@[0-9a-f]{40}$`)
	gitLogCommand     = regexp.MustCompile(`^(?:\[command\](?:/[^ ]+/)?git|git) (.*)$`)
	artifactFinalized = regexp.MustCompile(`^Artifact [A-Za-z0-9._-]{1,128} successfully finalized\. Artifact ID ([0-9]+)$`)
	artifactUploaded  = regexp.MustCompile(`^Artifact [A-Za-z0-9._-]{1,128} has been successfully uploaded! Final size is ([0-9]+) bytes\. Artifact ID is ([0-9]+)$`)
)

func hexadecimal(value string, n int) bool {
	if len(value) != n {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func pytestReportCommand(command string) (bool, string) {
	words := strings.Fields(command)
	start := 0
	switch {
	case len(words) > 0 && words[0] == "pytest":
		start = 1
	case len(words) > 3 && words[0] == "uv" && words[1] == "run" && words[2] == "--no-sync" && words[3] == "pytest":
		start = 4
	default:
		return false, ""
	}
	if strings.ContainsAny(command, ";&|<>$#`\\\"'") {
		return true, ""
	}
	path := ""
	count := 0
	for i := start; i < len(words); i++ {
		switch {
		case strings.HasPrefix(words[i], "--junitxml="):
			count++
			path = strings.TrimPrefix(words[i], "--junitxml=")
		case words[i] == "--junitxml":
			count++
			i++
			if i >= len(words) {
				return true, ""
			}
			path = words[i]
		}
	}
	if count != 1 || !validGoReportPath(path) {
		return true, ""
	}
	return true, path
}

// shortcut: measured checkout/upload log shape only, add fixtures before supporting other formats.
func verifyGitHubProvenance(log []byte, c githubProvenanceContext) (githubProvenance, error) {
	failure := func(reason string) (githubProvenance, error) {
		return githubProvenance{}, &githubProvenanceError{Reason: reason}
	}
	if len(log) > maxGitHubLogBytes {
		return failure("log_too_large")
	}
	if !c.LogComplete || c.RunID <= 0 || c.Attempt <= 0 || c.JobID <= 0 || c.ArtifactID <= 0 || c.JobRunID != c.RunID || c.JobAttempt != c.Attempt || c.LogJobID != c.JobID || c.ArtifactRunID != c.RunID || !validGoReportPath(c.Repository) || len(strings.Split(c.Repository, "/")) != 2 {
		return failure("invalid_context")
	}
	if !hexadecimal(c.ArchiveSHA256, 64) || c.ArtifactDigest != "sha256:"+c.ArchiveSHA256 || c.ArchiveSize <= 0 || c.ArtifactSize != c.ArchiveSize {
		return failure("artifact_context_mismatch")
	}
	if !utf8.Valid(log) || !bytes.HasSuffix(log, []byte("\n")) {
		return failure("unsupported_log")
	}
	derived := logANSI.ReplaceAllString(string(log), "")
	if strings.ContainsRune(derived, '\x1b') {
		return failure("unsupported_log")
	}
	phase := ""
	groups, checkouts, checkoutCommands, uploads, producers := 0, 0, 0, 0, 0
	repository, cwd, ref, testedSHA, uploadDigest, reportPath, echoCommand, lastBody := "", "", "", "", "", "", "", ""
	pendingSHA, finalized, uploaded := false, false, false
	testEcho, testScriptSafe, testEnv, failedTest := false, false, false, false
	for _, line := range strings.Split(strings.TrimSuffix(derived, "\n"), "\n") {
		stamp, body, ok := strings.Cut(line, " ")
		if !ok || len(line) > 64<<10 {
			return failure("unsupported_log")
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil || !strings.HasSuffix(stamp, "Z") {
			return failure("unsupported_log")
		}
		body = strings.TrimSuffix(body, "\r")
		lastBody = body
		if pendingSHA {
			if !hexadecimal(body, 40) {
				return failure("checkout_association")
			}
			testedSHA = body
			pendingSHA = false
			continue
		}
		if strings.HasPrefix(body, "##[error]") {
			if body != "##[error]Process completed with exit code 1." || phase != "test" || producers != 1 || reportPath == "" || testedSHA == "" || uploads != 0 || groups != 0 || !testEcho || !testScriptSafe || failedTest {
				return failure("failed_log")
			}
			failedTest = true
			continue
		}
		if strings.HasPrefix(body, "##[group]") {
			if strings.HasPrefix(body, "##[group]Run ") {
				if groups != 0 {
					return failure("incomplete_groups")
				}
				command := strings.TrimPrefix(body, "##[group]Run ")
				phase = "other"
				echoCommand = ""
				switch {
				case checkoutAction.MatchString(command):
					checkouts++
					if checkouts != 1 || uploads != 0 {
						return failure("checkout_ambiguity")
					}
					phase = "checkout"
				case uploadAction.MatchString(command):
					uploads++
					if uploads != 1 || testedSHA == "" {
						return failure("upload_association")
					}
					phase = "upload"
				case strings.HasPrefix(command, "actions/checkout@") || strings.HasPrefix(command, "actions/upload-artifact@"):
					return failure("unsupported_action")
				default:
					if recognized, path := pytestReportCommand(command); recognized {
						if testedSHA == "" || uploads != 0 {
							return failure("checkout_association")
						}
						producers++
						reportPath = path
						echoCommand = command
						phase = "test"
						testEcho, testScriptSafe, testEnv = false, true, false
					}
					if commandParts := gitLogCommand.FindStringSubmatch(command); commandParts != nil && revisionCommand(commandParts[1]) {
						return failure("later_revision_command")
					}
				}
			}
			groups++
			continue
		}
		if body == "##[endgroup]" {
			groups--
			if groups < 0 {
				return failure("incomplete_groups")
			}
			continue
		}

		if strings.HasPrefix(body, "Post ") {
			phase = "other"
			echoCommand = ""
		}
		if phase == "test" && groups > 0 {
			switch {
			case body == echoCommand && !testEcho:
				testEcho = true
			case testEcho && (strings.HasPrefix(body, "  shell: ") || strings.HasPrefix(body, "  working-directory: ")):
			case testEcho && body == "  env:":
				testEnv = true
			case testEcho && testEnv && strings.HasPrefix(body, "    ") && strings.Contains(body, ": "):
			default:
				testScriptSafe = false
			}
		}
		if strings.HasPrefix(body, "  repository: ") || strings.HasPrefix(body, "Syncing repository: ") {
			value := strings.TrimPrefix(strings.TrimPrefix(body, "  repository: "), "Syncing repository: ")
			if phase != "checkout" || value != c.Repository || (repository != "" && value != repository) {
				return failure("repository_mismatch")
			}
			repository = value
		}
		if strings.HasPrefix(body, "Working directory is '") && strings.HasSuffix(body, "'") {
			value := strings.TrimSuffix(strings.TrimPrefix(body, "Working directory is '"), "'")
			if phase != "checkout" || len(value) > 1024 || value == "" || (cwd != "" && value != cwd) {
				return failure("alternate_working_directory")
			}
			cwd = value
		}
		if strings.HasPrefix(strings.TrimSpace(body), "working-directory: ") {
			value := strings.TrimPrefix(strings.TrimSpace(body), "working-directory: ")
			if cwd == "" || value != cwd {
				return failure("alternate_working_directory")
			}
		}
		if commandParts := gitLogCommand.FindStringSubmatch(body); commandParts != nil {
			command := commandParts[1]
			if strings.Contains(" "+command+" ", " -C ") || strings.Contains(command, "--work-tree") || strings.Contains(command, "--git-dir") {
				return failure("alternate_working_directory")
			}
			if command == "log -1 --format=%H" {
				if phase != "checkout" || !strings.HasPrefix(body, "[command]/usr/bin/git ") || repository == "" || cwd == "" || checkoutCommands != 1 || testedSHA != "" {
					return failure("checkout_association")
				}
				pendingSHA = true
			} else if revisionCommand(command) {
				if phase != "checkout" || testedSHA != "" {
					return failure("later_revision_command")
				}
				if strings.Contains(" "+command+" ", " checkout ") {
					prefix := "checkout --progress --force "
					if !strings.HasPrefix(body, "[command]/usr/bin/git ") || !strings.HasPrefix(command, prefix) || checkoutCommands != 0 {
						return failure("checkout_ambiguity")
					}
					value := strings.TrimPrefix(command, prefix)
					if !strings.HasPrefix(value, "refs/") || !validGoReportPath(value) {
						return failure("unsupported_checkout_ref")
					}
					ref = value
					checkoutCommands++
				}
			}
		}
		if recognized, path := pytestReportCommand(body); recognized {
			if testedSHA == "" || uploads != 0 {
				return failure("checkout_association")
			}
			if body == echoCommand {
				echoCommand = ""
			} else {
				producers++
				reportPath = path
			}
		}
		if strings.HasPrefix(body, "SHA256 digest of uploaded artifact is ") {
			value := strings.TrimPrefix(body, "SHA256 digest of uploaded artifact is ")
			if phase != "upload" || uploadDigest != "" || value != c.ArchiveSHA256 {
				return failure("artifact_digest_mismatch")
			}
			uploadDigest = value
		}
		if match := artifactFinalized.FindStringSubmatch(body); match != nil {
			id, err := strconv.ParseInt(match[1], 10, 64)
			if phase != "upload" || finalized || uploadDigest == "" || err != nil || id != c.ArtifactID {
				return failure("artifact_id_mismatch")
			}
			finalized = true
		}
		if match := artifactUploaded.FindStringSubmatch(body); match != nil {
			size, sizeErr := strconv.ParseInt(match[1], 10, 64)
			id, idErr := strconv.ParseInt(match[2], 10, 64)
			if phase != "upload" || !finalized || uploaded || sizeErr != nil || idErr != nil || id != c.ArtifactID || size != c.ArchiveSize {
				return failure("artifact_upload_mismatch")
			}
			uploaded = true
		}
	}
	if groups != 0 || pendingSHA || lastBody != "Cleaning up orphan processes" {
		return failure("incomplete_log")
	}
	if checkouts != 1 || checkoutCommands != 1 || testedSHA == "" || ref == "" || uploads != 1 || uploadDigest == "" || !finalized || !uploaded {
		return failure("missing_evidence")
	}
	digest := sha256.Sum256(log)
	result := githubProvenance{TestedSHA: testedSHA, CheckoutRef: ref, LogSHA256: hex.EncodeToString(digest[:]), ProducerEvidence: fmt.Sprintf("github_run=%d;attempt=%d;job=%d;artifact=%d;zip_sha256=%s", c.RunID, c.Attempt, c.JobID, c.ArtifactID, c.ArchiveSHA256)}
	if producers == 1 && reportPath != "" {
		result.Language = "python"
		result.ReportPath = reportPath
	}
	return result, nil
}

func revisionCommand(command string) bool {
	for _, word := range strings.Fields(command) {
		switch word {
		case "checkout", "switch", "reset", "merge", "rebase", "cherry-pick", "pull", "fetch":
			return true
		}
	}
	return false
}
