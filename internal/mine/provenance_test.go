package mine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/omorsi45/hazard/internal/runner"
	"reflect"
	"strings"
	"testing"
)

func provenanceFixture() ([]byte, githubProvenanceContext) {
	bodies := []string{
		"##[group]Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		"  repository: owner/repo",
		"##[endgroup]",
		"Syncing repository: owner/repo",
		"##[group]Getting Git version info",
		"Working directory is '/home/runner/work/repo/repo'",
		"##[endgroup]",
		"##[group]Checking out the ref",
		"[command]/usr/bin/git checkout --progress --force refs/remotes/pull/17/merge",
		"##[endgroup]",
		"[command]/usr/bin/git log -1 --format=%H",
		"3329023300bb33fd3fa6430e2dafbb158deb0508",
		"##[group]Run uv run --no-sync pytest --junitxml=reports/junit.xml",
		"uv run --no-sync pytest --junitxml=reports/junit.xml",
		"  shell: /usr/bin/bash -e {0}",
		"##[endgroup]",
		"tests completed",
		"##[group]Run actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		"  path: reports/junit.xml",
		"##[endgroup]",
		"SHA256 digest of uploaded artifact is e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515",
		"Artifact test-results successfully finalized. Artifact ID 10715173308",
		"Artifact test-results has been successfully uploaded! Final size is 572407 bytes. Artifact ID is 10715173308",
		"Post job cleanup.",
		"Cleaning up orphan processes",
	}
	var out strings.Builder
	for _, body := range bodies {
		out.WriteString("2026-09-22T19:31:03.7731091Z ")
		out.WriteString(body)
		out.WriteByte('\n')
	}
	return []byte(out.String()), githubProvenanceContext{Repository: "owner/repo", RunID: 35774037511, JobRunID: 35774037511, Attempt: 1, JobAttempt: 1, JobID: 106902574731, LogJobID: 106902574731, ArtifactID: 10715173308, ArtifactRunID: 35774037511, ArchiveSHA256: "e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515", ArtifactDigest: "sha256:e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515", ArchiveSize: 572407, ArtifactSize: 572407, LogComplete: true}
}

func TestProvenanceSuccess(t *testing.T) {
	log, c := provenanceFixture()
	got, err := verifyGitHubProvenance(log, c)
	digest := sha256.Sum256(log)
	if err != nil || got.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || got.CheckoutRef != "refs/remotes/pull/17/merge" || got.LogSHA256 != hex.EncodeToString(digest[:]) || got.ProducerEvidence != "github_run=35774037511;attempt=1;job=106902574731;artifact=10715173308;zip_sha256=e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515" || got.Language != "python" || got.ReportPath != "reports/junit.xml" {
		t.Fatalf("supported log did not corroborate exact evidence: %+v %v", got, err)
	}
	if got.TestedSHA == "f54072d50807421e0d464df269e6c46a3e322a81" || got.TestedSHA == "3d3c42e5aac5ba805825da76410c181273ba90b1" {
		t.Fatal("source head or action version substituted for tested SHA")
	}
	ansi := bytes.Replace(log, []byte("3329023300bb33fd3fa6430e2dafbb158deb0508"), []byte("\x1b[32m3329023300bb33fd3fa6430e2dafbb158deb0508\x1b[0m"), 1)
	extracted, err := verifyGitHubProvenance(ansi, c)
	ansiDigest := sha256.Sum256(ansi)
	if err != nil || extracted.TestedSHA != got.TestedSHA || extracted.LogSHA256 != hex.EncodeToString(ansiDigest[:]) {
		t.Fatal("ANSI derivation lost raw-log digest or checkout evidence")
	}
}

func assertProvenanceRejected(t *testing.T, log []byte, c githubProvenanceContext) {
	t.Helper()
	got, err := verifyGitHubProvenance(log, c)
	var bounded *githubProvenanceError
	if err == nil || !errors.As(err, &bounded) || !reflect.DeepEqual(got, githubProvenance{}) {
		t.Fatal("unverified input corroborated provenance instead of bounded rejection")
	}
	if len(err.Error()) > 96 || strings.Contains(err.Error(), "SECRET_SENTINEL") || strings.Contains(err.Error(), "https://") {
		t.Fatal("provenance error exposed untrusted log or URL")
	}
}

func TestProvenanceContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*githubProvenanceContext)
	}{
		{"missing_run", func(c *githubProvenanceContext) { c.RunID = 0 }},
		{"missing_attempt", func(c *githubProvenanceContext) { c.Attempt = 0 }},
		{"missing_job", func(c *githubProvenanceContext) { c.JobID = 0 }},
		{"missing_artifact", func(c *githubProvenanceContext) { c.ArtifactID = 0 }},
		{"job_run", func(c *githubProvenanceContext) { c.JobRunID++ }},
		{"job_attempt", func(c *githubProvenanceContext) { c.JobAttempt++ }},
		{"log_job", func(c *githubProvenanceContext) { c.LogJobID++ }},
		{"artifact_run", func(c *githubProvenanceContext) { c.ArtifactRunID++ }},
		{"wrong_api_digest", func(c *githubProvenanceContext) { c.ArtifactDigest = "sha256:" + strings.Repeat("a", 64) }},
		{"invalid_download_digest", func(c *githubProvenanceContext) { c.ArchiveSHA256 = strings.Repeat("z", 64) }},
		{"wrong_api_size", func(c *githubProvenanceContext) { c.ArtifactSize++ }},
		{"missing_size", func(c *githubProvenanceContext) { c.ArchiveSize = 0; c.ArtifactSize = 0 }},
		{"partial_fetch", func(c *githubProvenanceContext) { c.LogComplete = false }},
		{"wrong_repository", func(c *githubProvenanceContext) { c.Repository = "other/repo" }},
	} {
		t.Run(tc.name, func(t *testing.T) { log, c := provenanceFixture(); tc.change(&c); assertProvenanceRejected(t, log, c) })
	}
}

func TestProvenanceGuards(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"missing_checkout", "actions/checkout@", "other/checkout@"},
		{"multiple_checkout", "##[group]Getting Git version info", "##[group]Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"},
		{"unsupported_checkout_ref", "refs/remotes/pull/17/merge", "SECRET_SENTINELhttps://host.invalid/?token=SECRET_SENTINEL"},
		{"missing_explicit_head", "[command]/usr/bin/git log -1 --format=%H", "unrelated version SHA"},
		{"missing_checkout_cwd", "Working directory is '/home/runner/work/repo/repo'", "directory unavailable"},
		{"oversized_checkout_cwd", "/home/runner/work/repo/repo", "/" + strings.Repeat("x", 1025)},
		{"git_cwd_during_checkout", "Syncing repository: owner/repo", "Syncing repository: owner/repo\n2026-09-22T19:31:03.7731091Z [command]/usr/bin/git -C /other fetch origin"},
		{"wrong_head_command", "[command]/usr/bin/git log -1 --format=%H", "[command]/usr/bin/git -C /other log -1 --format=%H"},
		{"head_outside_checkout", "[command]/usr/bin/git log -1 --format=%H", "##[group]Run echo later\n2026-09-22T19:31:03.7731091Z ##[endgroup]\n2026-09-22T19:31:03.7731091Z [command]/usr/bin/git log -1 --format=%H"},
		{"invalid_head", "3329023300bb33fd3fa6430e2dafbb158deb0508", "SECRET_SENTINELhttps://host.invalid/"},
		{"multiple_head", "tests completed", "[command]/usr/bin/git log -1 --format=%H\n2026-09-22T19:31:03.7731091Z f54072d50807421e0d464df269e6c46a3e322a81"},
		{"later_revision", "tests completed", "[command]/usr/bin/git switch main"},
		{"shell_revision", "tests completed", "git reset --hard f54072d50807421e0d464df269e6c46a3e322a81"},
		{"alternate_cwd", "  shell: /usr/bin/bash -e {0}", "  working-directory: /other"},
		{"multiple_repository", "Syncing repository: owner/repo", "Syncing repository: other/repo"},
		{"missing_upload", "actions/upload-artifact@", "other/upload-artifact@"},
		{"wrong_artifact", "10715173308", "10715173309"},
		{"conflicting_finalized_artifact", "successfully finalized. Artifact ID 10715173308", "successfully finalized. Artifact ID 10715173309"},
		{"conflicting_artifact", "Artifact ID is 10715173308", "Artifact ID is 10715173309"},
		{"wrong_upload_digest", "e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515", strings.Repeat("a", 64)},
		{"wrong_upload_size", "572407 bytes", "572408 bytes"},
		{"incomplete_upload", "Artifact test-results has been successfully uploaded! Final size is 572407 bytes. Artifact ID is 10715173308", "incomplete upload"},
		{"unclosed_group", "##[group]Checking out the ref", "##[group]Checking out the ref\n2026-09-22T19:31:03.7731091Z ##[group]extra"},
		{"unopened_group", "tests completed", "##[endgroup]"},
		{"oversized_line", "tests completed", strings.Repeat("x", 64<<10)},
		{"unsupported_escape", "tests completed", "tests completed\x1b]SECRET_SENTINEL"},
		{"multiple_uploader", "  path: reports/junit.xml", "##[endgroup]\n2026-09-22T19:31:03.7731091Z ##[group]Run actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a"},
		{"missing_footer", "Cleaning up orphan processes", "incomplete tail"},
		{"failed_log", "tests completed", "##[error]Process completed with exit code 2."},
		{"invalid_utf8", "tests completed", "\xff"},
		{"unsupported_timestamp", "2026-09-22T19:31:03.7731091Z ", "SECRET_SENTINELhttps://host.invalid/ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, c := provenanceFixture()
			assertProvenanceRejected(t, []byte(strings.ReplaceAll(string(log), tc.old, tc.new)), c)
		})
	}
	t.Run("missing_newline", func(t *testing.T) {
		log, c := provenanceFixture()
		assertProvenanceRejected(t, bytes.TrimSuffix(log, []byte("\n")), c)
	})
}

func TestProvenanceProducerLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
		wantLanguage   string
	}{
		{"plain_pytest", "uv run --no-sync pytest", "pytest", "python"},
		{"space_argument", "--junitxml=reports/junit.xml", "--junitxml reports/junit.xml", "python"},
		{"unsupported_wrapper", "uv run --no-sync pytest", "unknown-wrapper pytest", ""},
		{"no_report_path", "--junitxml=reports/junit.xml", "--verbose", ""},
		{"ambiguous_path", "--junitxml=reports/junit.xml", "--junitxml=reports/junit.xml --junitxml=other/junit.xml", ""},
		{"shell_expansion", "--junitxml=reports/junit.xml", "--junitxml=$REPORT", ""},
		{"shell_chain", "--junitxml=reports/junit.xml", "--junitxml=reports/junit.xml && echo done", ""},
		{"parent_path", "--junitxml=reports/junit.xml", "--junitxml=../junit.xml", ""},
		{"quoted_path", "--junitxml=reports/junit.xml", "--junitxml='reports/junit.xml'", ""},
		{"multiple_invocations", "tests completed", "##[group]Run pytest --junitxml=reports/junit.xml\n2026-09-22T19:31:03.7731091Z pytest --junitxml=reports/junit.xml\n2026-09-22T19:31:03.7731091Z ##[endgroup]", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, c := provenanceFixture()
			got, err := verifyGitHubProvenance([]byte(strings.ReplaceAll(string(log), tc.old, tc.new)), c)
			if err != nil || got.Language != tc.wantLanguage || (tc.wantLanguage == "" && got.ReportPath != "") {
				t.Fatal("ambiguous or unsupported producer syntax supplied automatic report language")
			}
			if got.Language != "" && got.ReportPath != "reports/junit.xml" {
				t.Fatal("producer changed exact report path")
			}
		})
	}
	log, c := provenanceFixture()
	got, err := verifyGitHubProvenance([]byte(strings.ReplaceAll(string(log), "uv run --no-sync pytest", "unknown-wrapper pytest")), c)
	if err != nil {
		t.Fatal("unsupported producer unexpectedly invalidated checkout/upload evidence")
	}
	raw := mineIdentity("", got.Language, "", runner.JUnitCase{Name: "test_value", Classname: "tests.test_value"})
	if raw.Identity.ID != "" {
		t.Fatal("missing report-linked language manufactured test identity")
	}
}

func TestProvenanceInputBound(t *testing.T) {
	log, c := provenanceFixture()
	padding := []byte("2026-09-22T19:31:03.7731091Z " + strings.Repeat("x", 256) + "\n")
	footer := []byte("2026-09-22T19:31:03.7731091Z Cleaning up orphan processes\n")
	oversized := bytes.TrimSuffix(log, footer)
	for len(oversized) <= 16<<20 {
		oversized = append(oversized, padding...)
	}
	oversized = append(oversized, footer...)
	assertProvenanceRejected(t, oversized, c)
}

func TestProvenanceProducerOrdering(t *testing.T) {
	log, c := provenanceFixture()
	log = bytes.ReplaceAll(log, []byte("uv run --no-sync pytest"), []byte("unknown-wrapper pytest"))
	log = bytes.Replace(log, []byte("Post job cleanup."), []byte("pytest --junitxml=reports/junit.xml"), 1)
	assertProvenanceRejected(t, log, c)
}

func TestProvenanceCompletedFailingTest(t *testing.T) {
	log, c := provenanceFixture()
	log = bytes.Replace(log, []byte("tests completed"), []byte("##[error]Process completed with exit code 1."), 1)
	got, err := verifyGitHubProvenance(log, c)
	if err != nil || got.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || got.Language != "python" || got.ReportPath != "reports/junit.xml" || got.ProducerEvidence == "" {
		t.Fatalf("complete failing test lost corroborated evidence: %v", err)
	}
	raw := runner.JUnitCase{Name: "test_value", Classname: "tests.test_value", Status: "fail", Message: "actual report assertion"}
	mapped := mineIdentity("", got.Language, "", raw)
	if mapped.Identity.ID != "py:tests.test_value:test_value" || !reflect.DeepEqual(mapped.Case, raw) {
		t.Fatal("failing report status or raw case was changed")
	}
}

func TestProvenanceFailingTestContext(t *testing.T) {
	const annotation = "##[error]Process completed with exit code 1."
	for _, tc := range []struct {
		name, old, new string
		failed         bool
	}{
		{"checkout_action", "Syncing repository: owner/repo", "Syncing repository: owner/repo\n2026-09-22T19:31:03.7731091Z " + annotation, false},
		{"before_test", "[command]/usr/bin/git log -1 --format=%H", annotation + "\n2026-09-22T19:31:03.7731091Z [command]/usr/bin/git log -1 --format=%H", false},
		{"uploader_action", "  path: reports/junit.xml", "  path: reports/junit.xml\n2026-09-22T19:31:03.7731091Z " + annotation, false},
		{"other_action", "tests completed", "##[group]Run echo another\n2026-09-22T19:31:03.7731091Z echo another\n2026-09-22T19:31:03.7731091Z ##[endgroup]\n2026-09-22T19:31:03.7731091Z " + annotation, false},
		{"post_action", "tests completed", "Post job cleanup.\n2026-09-22T19:31:03.7731091Z " + annotation, false},
		{"exit_two", "tests completed", "##[error]Process completed with exit code 2.", false},
		{"arbitrary_error", "tests completed", "##[error]SECRET_SENTINELhttps://host.invalid/?token=SECRET_SENTINEL", false},
		{"extra_script", "  shell: /usr/bin/bash -e {0}", "echo additional-command\n2026-09-22T19:31:03.7731091Z   shell: /usr/bin/bash -e {0}", true},
		{"repeated_script", "  shell: /usr/bin/bash -e {0}", "uv run --no-sync pytest --junitxml=reports/junit.xml\n2026-09-22T19:31:03.7731091Z   shell: /usr/bin/bash -e {0}", true},
		{"shell_composition", "--junitxml=reports/junit.xml", "--junitxml=reports/junit.xml && echo another-command", true},
		{"unknown_wrapper", "uv run --no-sync pytest", "unknown-wrapper pytest", true},
		{"ambiguous_path", "--junitxml=reports/junit.xml", "--junitxml=reports/junit.xml --junitxml=other/junit.xml", true},
		{"missing_uploader_summary", "Artifact test-results has been successfully uploaded! Final size is 572407 bytes. Artifact ID is 10715173308", "incomplete upload", true},
		{"missing_cleanup", "Cleaning up orphan processes", "incomplete tail", true},
		{"repeated_annotation", "tests completed", annotation + "\n2026-09-22T19:31:03.7731091Z " + annotation, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, c := provenanceFixture()
			if tc.failed {
				log = bytes.Replace(log, []byte("tests completed"), []byte(annotation), 1)
			}
			log = []byte(strings.ReplaceAll(string(log), tc.old, tc.new))
			assertProvenanceRejected(t, log, c)
		})
	}
}

func TestProvenanceCommentedReportSwitch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed bool
	}{
		{"successful_test", false},
		{"completed_failing_test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, c := provenanceFixture()
			log = bytes.ReplaceAll(log, []byte("pytest --junitxml=reports/junit.xml"), []byte("pytest # --junitxml=reports/junit.xml"))
			if tc.failed {
				log = bytes.Replace(log, []byte("tests completed"), []byte("##[error]Process completed with exit code 1."), 1)
				assertProvenanceRejected(t, log, c)
				return
			}
			got, err := verifyGitHubProvenance(log, c)
			if err != nil || got.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || got.ProducerEvidence == "" || got.Language != "" || got.ReportPath != "" {
				t.Fatal("commented report switch supplied automatic producer linkage or discarded corroborated checkout/upload evidence")
			}
		})
	}
}

func provenanceInputFixture() ([]byte, githubProvenanceContext) {
	log, c := provenanceFixture()
	const stamp = "2026-09-22T19:31:03.7731091Z "
	setup := stamp + "##[group]Run astral-sh/setup-uv@bec219d24cd3e171d82865faccec33120bb574f4\n" + stamp + "with:\n" + stamp + "  cache-dependency-glob: **/*requirements*.txt\n**/*requirements*.in\n**/uv.lock\n\n" + stamp + "  restore-cache: true\n" + stamp + "##[endgroup]\n"
	log = bytes.Replace(log, []byte(stamp+"##[group]Run uv run"), []byte(setup+stamp+"##[group]Run uv run"), 1)
	log = bytes.Replace(log, []byte(stamp+"  path: reports/junit.xml\n"), []byte(stamp+"with:\n"+stamp+"  path: reports/junit.xml\ncoverage.xml\ncoverage/\nf54072d50807421e0d464df269e6c46a3e322a81\n\n"), 1)
	return log, c
}

func TestProvenanceActionInputContinuations(t *testing.T) {
	log, c := provenanceInputFixture()
	got, err := verifyGitHubProvenance(log, c)
	digest := sha256.Sum256(log)
	if err != nil || got.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || got.CheckoutRef != "refs/remotes/pull/17/merge" || got.LogSHA256 != hex.EncodeToString(digest[:]) || got.ProducerEvidence != "github_run=35774037511;attempt=1;job=106902574731;artifact=10715173308;zip_sha256=e88639bc11ac82b50d7d908ba1b596b43a99c8aa3cdcf16447332033cb11c515" || got.Language != "python" || got.ReportPath != "reports/junit.xml" {
		t.Fatalf("measured opaque action input continuations lost exact provenance: %v", err)
	}
}

func TestProvenanceLeadingBOM(t *testing.T) {
	log, c := provenanceFixture()
	log = append([]byte{0xef, 0xbb, 0xbf}, log...)
	got, err := verifyGitHubProvenance(log, c)
	digest := sha256.Sum256(log)
	if err != nil || got.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || got.LogSHA256 != hex.EncodeToString(digest[:]) || got.ProducerEvidence == "" || got.Language != "python" || got.ReportPath != "reports/junit.xml" {
		t.Fatalf("one leading BOM lost original digest or exact producer: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"double_prefix", func(b []byte) []byte { return append([]byte{0xef, 0xbb, 0xbf}, b...) }},
		{"nonleading_prefix", func(b []byte) []byte { return bytes.Replace(b, []byte("\n"), []byte("\n\xef\xbb\xbf"), 1) }},
		{"truncated", func(b []byte) []byte { return bytes.TrimSuffix(b, []byte("\n")) }},
		{"invalid_utf8", func(b []byte) []byte { return bytes.Replace(b, []byte("tests completed"), []byte{0xff}, 1) }},
		{"incomplete", func(b []byte) []byte {
			return bytes.Replace(b, []byte("Cleaning up orphan processes"), []byte("incomplete tail"), 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertProvenanceRejected(t, tc.change(log), c) })
	}
	t.Run("context_mismatch", func(t *testing.T) { c.ArtifactRunID++; assertProvenanceRejected(t, log, c) })
}

func TestProvenanceInputContinuationGuards(t *testing.T) {
	const stamp = "2026-09-22T19:31:03.7731091Z "
	for _, tc := range []struct{ name, old, new string }{
		{"orphan", stamp + "##[group]Run actions/checkout@", "**/uv.lock\n" + stamp + "##[group]Run actions/checkout@"},
		{"other_input", "  cache-dependency-glob:", "  other-input:"},
		{"other_action", "astral-sh/setup-uv@", "owner/action@"},
		{"unpinned_action", "astral-sh/setup-uv@bec219d24cd3e171d82865faccec33120bb574f4", "astral-sh/setup-uv@v7"},
		{"outside_with", stamp + "with:\n" + stamp + "  cache-dependency-glob:", stamp + "not-with:\n" + stamp + "  cache-dependency-glob:"},
		{"other_section", stamp + "with:\n" + stamp + "  cache-dependency-glob:", stamp + "env:\n" + stamp + "  cache-dependency-glob:"},
		{"closed_group", "**/*requirements*.in\n", stamp + "##[endgroup]\n**/*requirements*.in\n"},
		{"nested_group", stamp + "  cache-dependency-glob:", stamp + "##[group]nested\n" + stamp + "  cache-dependency-glob:"},
		{"next_timestamp", "**/*requirements*.in\n", stamp + "  restore-cache: true\n**/*requirements*.in\n"},
		{"left_with_header", stamp + "  cache-dependency-glob:", stamp + "action output\n" + stamp + "  cache-dependency-glob:"},
		{"test_script", stamp + "uv run --no-sync pytest --junitxml=reports/junit.xml\n", stamp + "uv run --no-sync pytest --junitxml=reports/junit.xml\n**/uv.lock\n"},
		{"command", "**/*requirements*.in", "git switch main"},
		{"control_marker", "**/*requirements*.in", "##[endgroup]"},
		{"embedded_BOM", "**/*requirements*.in", "\xef\xbb\xbf**/uv.lock"},
		{"oversized_line", "**/*requirements*.in", strings.Repeat("x", (64<<10)+1)},
		{"pending_checkout_sha", stamp + "3329023300bb33fd3fa6430e2dafbb158deb0508\n", "3329023300bb33fd3fa6430e2dafbb158deb0508\n"},
		{"upload_outside_with", stamp + "with:\n" + stamp + "  path:", stamp + "not-with:\n" + stamp + "  path:"},
		{"upload_other_input", "  path: reports/junit.xml", "  other-input: reports/junit.xml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, c := provenanceInputFixture()
			assertProvenanceRejected(t, []byte(strings.ReplaceAll(string(log), tc.old, tc.new)), c)
		})
	}
}

func TestProvenancePhysicalContinuationGuards(t *testing.T) {
	for _, tc := range []struct{ name, replacement string }{
		{"ansi_control_token", "\x1b[32m**/*requirements*.in\x1b[0m"},
		{"oversized_physical_line", strings.Repeat("\x1b[0m", (64<<10)/4+1) + "**/*requirements*.in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, c := provenanceInputFixture()
			log = bytes.Replace(log, []byte("**/*requirements*.in"), []byte(tc.replacement), 1)
			assertProvenanceRejected(t, log, c)
		})
	}
	t.Run("oversized_timestamped_line", func(t *testing.T) {
		log, c := provenanceInputFixture()
		log = bytes.Replace(log, []byte("tests completed"), []byte(strings.Repeat("\x1b[0m", (64<<10)/4+1)+"tests completed"), 1)
		got, err := verifyGitHubProvenance(log, c)
		var bounded *githubProvenanceError
		if !errors.As(err, &bounded) || bounded.Reason != "unsupported_log" || !reflect.DeepEqual(got, githubProvenance{}) {
			t.Fatal("oversized timestamped physical line bypassed original byte bound")
		}
	})
	t.Run("CRLF_framing", func(t *testing.T) {
		log, c := provenanceInputFixture()
		log = bytes.ReplaceAll(log, []byte("\n"), []byte("\r\n"))
		got, err := verifyGitHubProvenance(log, c)
		digest := sha256.Sum256(log)
		if err != nil || got.TestedSHA != "3329023300bb33fd3fa6430e2dafbb158deb0508" || got.LogSHA256 != hex.EncodeToString(digest[:]) || got.ReportPath != "reports/junit.xml" {
			t.Fatalf("CRLF framing lost opaque continuations or original digest: %v", err)
		}
	})
}
