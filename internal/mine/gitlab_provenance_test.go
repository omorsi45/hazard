package mine

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const gitLabFixtureSHA = "0123456789abcdef0123456789abcdef01234567"

func gitLabFixtureContext() gitlabProvenanceContext {
	return gitlabProvenanceContext{
		ProjectID: 7, PipelineID: 11, JobID: 42, JobProjectID: 7,
		JobPipelineID: 11, TraceJobID: 42, JobStatus: "success",
		TraceComplete: true, ReportValidated: true, ReportSHA256: strings.Repeat("a", 64),
	}
}

func gitLabFixtureTrace() string {
	return "Running with gitlab-runner 19.4.0 (fixture)\n" +
		"section_start:10:prepare_executor\r\x1b[0KPreparing the executor\n" +
		"ordinary preparation\nsection_end:11:prepare_executor\r\x1b[0K\n" +
		"section_start:12:step_script\r\x1b[0KExecuting \"step_script\" stage of the job script\n" +
		"$ git rev-parse HEAD\n" + gitLabFixtureSHA + "\n" +
		"$ pytest --junitxml=reports/result.xml\nopaque test output\n" +
		"section_end:13:step_script\r\x1b[0K\n" +
		"section_start:14:upload_artifacts_on_success\r\x1b[0KUploading artifacts for successful job\n" +
		"Uploading artifacts...\n" +
		"reports/result.xml: found 1 matching artifact files and directories\n" +
		"Uploading artifacts as \"junit\" to coordinator... 201 Created id=42 status=201 token=fixture-secret\n" +
		"section_end:15:upload_artifacts_on_success\r\x1b[0K\n" +
		"section_start:16:cleanup_file_variables\r\x1b[0KCleaning up project directory and file based variables\n" +
		"section_end:17:cleanup_file_variables\r\x1b[0K\nJob succeeded\n"
}

func gitLabReject(t *testing.T, trace string, c gitlabProvenanceContext) {
	t.Helper()
	got, err := verifyGitLabProvenance([]byte(trace), c)
	if err == nil {
		t.Fatal("unsupported evidence accepted")
	}
	if got != (gitlabProvenance{}) {
		t.Fatal("rejected evidence retained claims")
	}
	var safe *gitlabProvenanceError
	if !errors.As(err, &safe) || safe.Reason == "" || len(err.Error()) > 100 {
		t.Fatal("rejection did not return a bounded provenance reason")
	}
	for _, secret := range []string{"fixture-secret", "credential-canary", "https://private.example", gitLabFixtureSHA, "reports/result.xml"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("rejection exposed source content")
		}
	}
}

func TestGitLabProvenanceSuccess(t *testing.T) {
	for _, variant := range []string{"success", "ordinary_failure", "ansi", "field_order", "opaque_stdout", "heading", "runtime"} {
		t.Run(variant, func(t *testing.T) {
			trace, c := gitLabFixtureTrace(), gitLabFixtureContext()
			switch variant {
			case "ordinary_failure":
				c.JobStatus, c.FailureReason = "failed", "script_failure"
				trace = strings.ReplaceAll(trace, "upload_artifacts_on_success", "upload_artifacts_on_failure")
				trace = strings.Replace(trace, "Job succeeded", "ERROR: Job failed: exit code 1", 1)
			case "ansi":
				trace = "\x1b[0K" + strings.ReplaceAll(trace, "section_start:", "\x1b[0Ksection_start:")
				trace = strings.Replace(trace, "$ git rev-parse HEAD", "\x1b[32;1m$ git rev-parse HEAD\x1b[0;m", 1)
				trace = strings.Replace(trace, "Job succeeded", "\x1b[32;1mJob succeeded\x1b[0;m", 1)
			case "field_order":
				trace = strings.Replace(trace, "id=42 status=201 token=fixture-secret", "token=fixture-secret   status=201\tid=42", 1)
			case "opaque_stdout":
				trace = strings.Replace(trace, "opaque test output", "fixture-secret https://private.example\n"+strings.Repeat("b", 40)+"\nUploading artifacts as fake test output", 1)
			case "runtime":
				trace = strings.Replace(trace, "Uploading artifacts...\n", "Uploading artifacts...\nRuntime platform arch=amd64 os=linux pid=123 revision=abc123 version=19.4.0\n", 1)
			case "heading":
				trace = strings.Replace(trace, "Executing \"step_script\" stage of the job script", "irrelevant human heading", 1)
			}
			got, err := verifyGitLabProvenance([]byte(trace), c)
			if err != nil {
				t.Fatalf("supported recorded execution rejected: %v", err)
			}
			if got.TestedSHA != gitLabFixtureSHA || got.Language != "python" || got.ReportPath != "reports/result.xml" {
				t.Fatal("measured HEAD or sole producer path was not preserved")
			}
			if got.LogSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(trace))) {
				t.Fatal("raw trace digest was not preserved")
			}
			if !strings.Contains(got.ProducerEvidence, "project=7 pipeline=11 job=42") || !strings.Contains(got.ProducerEvidence, "report_sha256="+strings.Repeat("a", 64)) || !strings.Contains(got.ProducerEvidence, "method=gitlab-runner-pytest-junit-v1") {
				t.Fatal("validated context or supported method missing from evidence")
			}
			if len(got.ProducerEvidence) > 250 || strings.Contains(got.ProducerEvidence, "fixture-secret") || strings.Contains(got.ProducerEvidence, "private.example") || strings.Contains(got.ProducerEvidence, "reports/result.xml") {
				t.Fatal("producer evidence retained raw source text")
			}
		})
	}
}

func TestGitLabProvenanceContext(t *testing.T) {
	for _, field := range []string{"project", "pipeline", "job", "job_project", "job_pipeline", "trace_job"} {
		for _, bad := range []int64{0, -1} {
			t.Run(fmt.Sprintf("%s_%d", field, bad), func(t *testing.T) {
				c := gitLabFixtureContext()
				switch field {
				case "project":
					c.ProjectID = bad
				case "pipeline":
					c.PipelineID = bad
				case "job":
					c.JobID = bad
				case "job_project":
					c.JobProjectID = bad
				case "job_pipeline":
					c.JobPipelineID = bad
				case "trace_job":
					c.TraceJobID = bad
				}
				gitLabReject(t, gitLabFixtureTrace(), c)
			})
		}
	}
	for _, field := range []string{"job_project", "job_pipeline", "trace_job", "trace_complete", "report_validated", "digest_empty", "digest_short", "digest_upper", "digest_nonhex", "success_failure_reason", "failed_no_reason", "system_failure", "timeout", "running", "cancelled"} {
		t.Run(field, func(t *testing.T) {
			c := gitLabFixtureContext()
			switch field {
			case "job_project":
				c.JobProjectID++
			case "job_pipeline":
				c.JobPipelineID++
			case "trace_job":
				c.TraceJobID++
			case "trace_complete":
				c.TraceComplete = false
			case "report_validated":
				c.ReportValidated = false
			case "digest_empty":
				c.ReportSHA256 = ""
			case "digest_short":
				c.ReportSHA256 = strings.Repeat("a", 63)
			case "digest_upper":
				c.ReportSHA256 = strings.Repeat("A", 64)
			case "digest_nonhex":
				c.ReportSHA256 = strings.Repeat("x", 64)
			case "success_failure_reason":
				c.FailureReason = "runner_system_failure"
			case "failed_no_reason":
				c.JobStatus = "failed"
			case "system_failure":
				c.JobStatus, c.FailureReason = "failed", "runner_system_failure"
			case "timeout":
				c.JobStatus, c.FailureReason = "failed", "job_execution_timeout"
			case "running":
				c.JobStatus = "running"
			case "cancelled":
				c.JobStatus = "canceled"
			}
			gitLabReject(t, gitLabFixtureTrace(), c)
		})
	}
}

func TestGitLabProvenanceFraming(t *testing.T) {
	base := gitLabFixtureTrace()
	cases := map[string]string{
		"missing_header":       strings.TrimPrefix(base, "Running with gitlab-runner 19.4.0 (fixture)\n"),
		"empty":                "",
		"no_final_newline":     strings.TrimSuffix(base, "\n"),
		"invalid_utf8":         strings.Replace(base, "opaque test output", "\xff", 1),
		"unknown_escape":       strings.Replace(base, "opaque test output", "\x1b[2J", 1),
		"unknown_control":      strings.Replace(base, "opaque test output", "\x00", 1),
		"ordinary_cr":          strings.Replace(base, "opaque test output", "hidden\rcontent", 1),
		"marker_missing_cr":    strings.Replace(base, "step_script\r\x1b[0K", "step_script\x1b[0K", 1),
		"marker_missing_clear": strings.Replace(base, "step_script\r\x1b[0K", "step_script\r", 1),
		"timestamp_overflow":   strings.Replace(base, "section_start:12:", "section_start:18446744073709551616:", 1),
		"timestamp_prefix":     strings.Replace(base, "$ git rev-parse HEAD", "2026-10-10T12:00:00Z $ git rev-parse HEAD", 1),
		"nested_stage":         strings.Replace(base, "opaque test output", "section_start:12:step_script\r\x1b[0K", 1),
		"unbalanced_stage":     strings.Replace(base, "section_end:13:step_script\r\x1b[0K\n", "", 1),
		"wrong_end_stage":      strings.Replace(base, "section_end:13:step_script", "section_end:13:get_sources", 1),
		"custom_stage":         strings.ReplaceAll(base, "prepare_executor", "custom_section"),
		"prep_echoed_command":  strings.Replace(base, "ordinary preparation", "$ git reset --hard other", 1),
		"prep_fake_stage":      strings.Replace(base, "ordinary preparation", "section_start:11:step_script\r\x1b[0K", 1),
		"after_script":         strings.Replace(base, "section_start:14:", "section_start:13:after_script\r\x1b[0K\nsection_end:13:after_script\r\x1b[0K\nsection_start:14:", 1),
		"command_after_upload": strings.Replace(base, "section_start:16:", "$ git checkout other\nsection_start:16:", 1),
		"missing_cleanup":      strings.Replace(base, "section_end:17:cleanup_file_variables\r\x1b[0K\n", "", 1),
		"missing_terminal":     strings.TrimSuffix(base, "Job succeeded\n"),
		"extra_after_terminal": base + "extra output\n",
		"wrong_terminal":       strings.Replace(base, "Job succeeded", "ERROR: Job failed: exit code 1", 1),
		"truncated_notice":     strings.Replace(base, "opaque test output", "Job's log exceeded limit of 4194304 bytes", 1),
		"loss_notice":          strings.Replace(base, "opaque test output", "WARNING: trace data lost", 1),
		"heading_command":      strings.Replace(base, "Executing \"step_script\" stage of the job script", "$ git checkout other", 1),
	}
	for name, trace := range cases {
		t.Run(name, func(t *testing.T) { gitLabReject(t, trace, gitLabFixtureContext()) })
	}
}

func TestGitLabProvenanceBounds(t *testing.T) {
	base := gitLabFixtureTrace()
	for _, n := range []int{65536, 65537} {
		t.Run(fmt.Sprintf("line_%d", n), func(t *testing.T) {
			trace := strings.Replace(base, "opaque test output", strings.Repeat("x", n), 1)
			if n == 65537 {
				gitLabReject(t, trace, gitLabFixtureContext())
				return
			}
			if _, err := verifyGitLabProvenance([]byte(trace), gitLabFixtureContext()); err != nil {
				t.Fatalf("bounded line rejected: %v", err)
			}
		})
	}
	for _, n := range []int{16 * 1024 * 1024, 16*1024*1024 + 1} {
		t.Run(fmt.Sprintf("trace_%d", n), func(t *testing.T) {
			remaining := n - len(base)
			var padding strings.Builder
			for remaining > 65000 {
				padding.WriteString(strings.Repeat("x", 64999) + "\n")
				remaining -= 65000
			}
			padding.WriteString(strings.Repeat("x", remaining-1) + "\n")
			trace := strings.Replace(base, "opaque test output\n", "opaque test output\n"+padding.String(), 1)
			if len(trace) != n {
				t.Fatal("size fixture mismatch")
			}
			if n > 16*1024*1024 {
				gitLabReject(t, trace, gitLabFixtureContext())
				return
			}
			if _, err := verifyGitLabProvenance([]byte(trace), gitLabFixtureContext()); err != nil {
				t.Fatalf("bounded trace rejected: %v", err)
			}
		})
	}
}

func TestGitLabProvenanceCommands(t *testing.T) {
	base := gitLabFixtureTrace()
	cases := map[string]string{
		"reversed":         strings.Replace(base, "$ git rev-parse HEAD\n"+gitLabFixtureSHA+"\n$ pytest --junitxml=reports/result.xml", "$ pytest --junitxml=reports/result.xml\n$ git rev-parse HEAD\n"+gitLabFixtureSHA, 1),
		"short_sha":        strings.Replace(base, gitLabFixtureSHA, gitLabFixtureSHA[:12], 1),
		"upper_sha":        strings.Replace(base, gitLabFixtureSHA, strings.ToUpper(gitLabFixtureSHA), 1),
		"conflicting_sha":  strings.Replace(base, gitLabFixtureSHA, gitLabFixtureSHA+"\n"+strings.Repeat("b", 40), 1),
		"unrelated_output": strings.Replace(base, gitLabFixtureSHA, "ordinary output\n"+gitLabFixtureSHA, 1),
		"variable_sha":     strings.Replace(base, "$ git rev-parse HEAD", "$ echo $CI_COMMIT_SHA", 1),
		"extra_command":    strings.Replace(base, "opaque test output", "$ echo extra", 1),
		"collapsed":        strings.Replace(base, "opaque test output", "# collapsed multi-line command", 1),
		"wrapped":          strings.Replace(base, "$ pytest --junitxml=", "$ python -m pytest --junitxml=", 1),
		"flags":            strings.Replace(base, "$ pytest --junitxml=reports/result.xml", "$ pytest -q --junitxml=reports/result.xml", 1),
		"chained":          strings.Replace(base, "$ pytest --junitxml=reports/result.xml", "$ pytest --junitxml=reports/result.xml; echo other", 1),
	}
	for name, trace := range cases {
		t.Run(name, func(t *testing.T) { gitLabReject(t, trace, gitLabFixtureContext()) })
	}
	for _, path := range []string{"", "/result.xml", "../result.xml", "./result.xml", "reports/../result.xml", "reports//result.xml", "reports/./result.xml", "reports/*.xml", "$REPORT.xml", "'result.xml'", "result.xml && true", "result.xml extra", "C:/result.xml", "reports\\result.xml", "result.txt", "résult.xml"} {
		t.Run("path_"+path, func(t *testing.T) {
			trace := strings.ReplaceAll(base, "reports/result.xml", path)
			gitLabReject(t, trace, gitLabFixtureContext())
		})
	}
}

func TestGitLabProvenanceUpload(t *testing.T) {
	base := gitLabFixtureTrace()
	cases := map[string]string{
		"wrong_path":        strings.Replace(base, "reports/result.xml: found", "result.xml: found", 1),
		"wrong_count":       strings.Replace(base, "found 1 matching", "found 2 matching", 1),
		"zero_count":        strings.Replace(base, "found 1 matching", "found 0 matching", 1),
		"count_overflow":    strings.Replace(base, "found 1 matching", "found 18446744073709551616 matching", 1),
		"extra_path":        strings.Replace(base, "reports/result.xml: found", "other.xml: found 1 matching artifact files and directories\nreports/result.xml: found", 1),
		"archive_type":      strings.Replace(base, "as \"junit\"", "as \"archive\"", 1),
		"wrong_job":         strings.Replace(base, "id=42", "id=43", 1),
		"job_overflow":      strings.Replace(base, "id=42", "id=9223372036854775808", 1),
		"wrong_status":      strings.Replace(base, "status=201", "status=200", 1),
		"missing_status":    strings.Replace(base, " status=201", "", 1),
		"older_formatter":   strings.Replace(base, "status=201", "responseStatus=201", 1),
		"duplicate_status":  strings.Replace(base, "status=201", "status=201 status=201", 1),
		"duplicate_job":     strings.Replace(base, "id=42", "id=42 id=43", 1),
		"second_uploader":   strings.Replace(base, "section_end:15:", "Uploading artifacts...\nsection_end:15:", 1),
		"missing_block":     strings.Replace(base, "Uploading artifacts...\n", "", 1),
		"missing_success":   strings.Replace(base, "Uploading artifacts as \"junit\" to coordinator... 201 Created id=42 status=201 token=fixture-secret\n", "", 1),
		"reversed_records":  strings.Replace(base, "reports/result.xml: found 1 matching artifact files and directories\nUploading artifacts as \"junit\" to coordinator... 201 Created id=42 status=201 token=fixture-secret", "Uploading artifacts as \"junit\" to coordinator... 201 Created id=42 status=201\nreports/result.xml: found 1 matching artifact files and directories", 1),
		"untracked":         strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nuntracked: found 1 files\n", 1),
		"excluded":          strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nexcluded: 1 files\n", 1),
		"warning":           strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nWARNING: no matching files\n", 1),
		"retry":             strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nRetrying upload\n", 1),
		"redirect":          strings.Replace(base, "201 Created", "redirected to 201 Created", 1),
		"wrong_stage":       strings.ReplaceAll(base, "upload_artifacts_on_success", "upload_artifacts_on_failure"),
		"duplicate_runtime": strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nRuntime platform arch=amd64 os=linux pid=123 revision=abc123 version=19.4.0\nRuntime platform arch=amd64 os=linux pid=123 revision=abc123 version=19.4.0\n", 1),
		"runtime_injection": strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nRuntime platform arch=amd64 os=linux pid=123 revision=abc123 version=19.4.0 id=42\n", 1),
	}
	for name, trace := range cases {
		t.Run(name, func(t *testing.T) { gitLabReject(t, trace, gitLabFixtureContext()) })
	}
}

func TestGitLabProvenanceFailureAndSecrets(t *testing.T) {
	trace := strings.ReplaceAll(gitLabFixtureTrace(), "upload_artifacts_on_success", "upload_artifacts_on_failure")
	c := gitLabFixtureContext()
	c.JobStatus, c.FailureReason = "failed", "script_failure"
	for _, terminal := range []string{"Job succeeded", "ERROR: Job failed: exit code 2", "ERROR: Job failed (system failure): exit code 1", "ERROR: Job failed: execution took longer than timeout"} {
		t.Run(terminal, func(t *testing.T) { gitLabReject(t, strings.Replace(trace, "Job succeeded", terminal, 1), c) })
	}
	t.Run("secret_trace", func(t *testing.T) {
		gitLabReject(t, "credential-canary https://private.example\n", gitLabFixtureContext())
	})
	t.Run("secret_context", func(t *testing.T) {
		c := gitLabFixtureContext()
		c.JobStatus, c.ReportSHA256 = "credential-canary", "https://private.example"
		gitLabReject(t, gitLabFixtureTrace(), c)
	})
}

func TestGitLabProvenanceControlAndTerminal(t *testing.T) {
	base := gitLabFixtureTrace()
	for name, trace := range map[string]string{
		"runtime_value":        strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nRuntime platform arch=amd/64 os=linux pid=123 revision=abc123 version=19.4.0\n", 1),
		"unknown_color":        strings.Replace(base, "opaque test output", "\x1b[2mtext\x1b[0m", 1),
		"signed_color":         strings.Replace(base, "opaque test output", "\x1b[+1mtext\x1b[0m", 1),
		"negative_color":       strings.Replace(base, "opaque test output", "\x1b[-0mtext\x1b[0m", 1),
		"error_success":        strings.Replace(base, "Job succeeded", "ERROR: Job succeeded", 1),
		"repeated_preparation": strings.Replace(base, "section_start:12:", "section_start:11:prepare_executor\r\x1b[0K\nsection_end:11:prepare_executor\r\x1b[0K\nsection_start:12:", 1),
		"late_preparation":     strings.Replace(base, "section_start:14:", "section_start:13:get_sources\r\x1b[0K\nsection_end:13:get_sources\r\x1b[0K\nsection_start:14:", 1),
		"late_runtime":         strings.Replace(base, "Uploading artifacts as \"junit\"", "Runtime platform arch=amd64 os=linux pid=123 revision=abc123 version=19.4.0\nUploading artifacts as \"junit\"", 1),
		"malformed_runtime":    strings.Replace(base, "Uploading artifacts...\n", "Uploading artifacts...\nRuntime platform arch=amd64 os=linux pid=invalid revision=abc123 version=19.4.0\n", 1),
	} {
		t.Run(name, func(t *testing.T) { gitLabReject(t, trace, gitLabFixtureContext()) })
	}
}

func TestGitLabProvenanceBindingsAndCompleteness(t *testing.T) {
	t.Run("nonpositive_associated_project", func(t *testing.T) {
		c := gitLabFixtureContext()
		c.ProjectID, c.JobProjectID = 0, 0
		gitLabReject(t, gitLabFixtureTrace(), c)
	})
	t.Run("duplicate_terminal", func(t *testing.T) { gitLabReject(t, gitLabFixtureTrace()+"Job succeeded\n", gitLabFixtureContext()) })
	t.Run("wrong_header", func(t *testing.T) {
		gitLabReject(t, strings.Replace(gitLabFixtureTrace(), "Running with gitlab-runner", "Unrecognized runner", 1), gitLabFixtureContext())
	})
	for _, reason := range []string{"runner_system_failure", "job_execution_timeout", ""} {
		t.Run("failed_"+reason, func(t *testing.T) {
			c := gitLabFixtureContext()
			c.JobStatus, c.FailureReason = "failed", reason
			trace := strings.ReplaceAll(gitLabFixtureTrace(), "upload_artifacts_on_success", "upload_artifacts_on_failure")
			trace = strings.Replace(trace, "Job succeeded", "ERROR: Job failed: exit code 1", 1)
			gitLabReject(t, trace, c)
		})
	}
}

func TestGitLabProvenanceUploadFieldPresence(t *testing.T) {
	base := gitLabFixtureTrace()
	for name, trace := range map[string]string{
		"missing_job":        strings.Replace(base, " id=42", "", 1),
		"duplicate_same_job": strings.Replace(base, "id=42", "id=42 id=42", 1),
		"truncated_word":     strings.Replace(base, "opaque test output", "trace truncated", 1),
	} {
		t.Run(name, func(t *testing.T) { gitLabReject(t, trace, gitLabFixtureContext()) })
	}
}

func TestGitLabProvenanceUploadSingleLine(t *testing.T) {
	for _, extra := range []string{" Uploading artifacts...", " other.xml: found 1 matching artifact files and directories"} {
		t.Run(extra, func(t *testing.T) {
			trace := strings.Replace(gitLabFixtureTrace(), "token=fixture-secret", "token=fixture-secret"+extra, 1)
			gitLabReject(t, trace, gitLabFixtureContext())
		})
	}
}
