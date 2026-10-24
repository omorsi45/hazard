package mine

import (
	"errors"
	"strconv"
	"strings"
	"text/scanner"

	"github.com/omorsi45/hazard/internal/runner"
)

func mineIdentity(explicitLanguage, producerLanguage, testedModule string, c runner.JUnitCase) ObservationCase {
	result := ObservationCase{Case: c}
	if producerLanguage == "unknown" {
		producerLanguage = ""
	}
	if (explicitLanguage != "" && explicitLanguage != "go" && explicitLanguage != "python") || (producerLanguage != "" && producerLanguage != "go" && producerLanguage != "python") {
		result.IdentityError = "unsupported_language"
		return result
	}
	if explicitLanguage != "" && producerLanguage != "" && explicitLanguage != producerLanguage {
		result.IdentityError = "language_conflict"
		return result
	}
	language := explicitLanguage
	evidence := ""
	if language != "" {
		evidence = "operator_language=" + language
	}
	if producerLanguage != "" {
		if evidence != "" {
			evidence += ";"
		}
		evidence += "report_producer_language=" + producerLanguage
		language = producerLanguage
	}
	if language == "" {
		result.IdentityError = "language_unavailable"
		return result
	}
	hint := ""
	if language == "go" {
		firstDiagnostic, _, _ := strings.Cut(c.Message, "\n")
		if c.Status == "error" && c.Type == "" && (firstDiagnostic == "Build error" || (c.Name == "Failure" && firstDiagnostic == "Runtime error")) {
			result.IdentityError = "synthetic_go_error_record"
			result.InfraReason = "synthetic_go_error_record"
			return result
		}
		if !validGoReportPath(testedModule) || !validGoReportPath(c.Classname) || (c.Classname != testedModule && !strings.HasPrefix(c.Classname, testedModule+"/")) {
			result.IdentityError = "go_package_unavailable"
			return result
		}
		hint = c.Classname
		evidence += ";tested_root_module_agreement"
	}
	identity, err := runner.NormalizeJUnitCase(language, hint, c)
	if err != nil {
		result.IdentityError = "unsupported_test_identity"
		return result
	}
	result.Identity = identity
	result.IdentityEvidence = evidence
	return result
}

const maxGoModuleBytes = 1 << 20

func validGoReportPath(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || part == "..." {
			return false
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._-", r) {
				return false
			}
		}
	}
	return true
}

// shortcut: supported module directive lexing only, use a reviewed parser if broader syntax is required.
func readGoModule(data []byte) (string, error) {
	if len(data) > maxGoModuleBytes {
		return "", errors.New("go_module_too_large")
	}
	var lexer scanner.Scanner
	lexer.Init(strings.NewReader(string(data)))
	lexer.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanComments | scanner.SkipComments
	lexer.IsIdentRune = func(r rune, i int) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) || (i > 0 && r == '/')
	}
	lexicalError := false
	lexer.Error = func(*scanner.Scanner, string) { lexicalError = true }
	type moduleToken struct {
		text string
		kind rune
	}
	var tokens []moduleToken
	line, depth := 0, 0
	module := ""
	process := func() error {
		if len(tokens) > 0 && depth == 0 && tokens[0].text == "module" {
			if module != "" {
				return errors.New("go_module_ambiguous")
			}
			if len(tokens) != 2 {
				return errors.New("go_module_unsupported")
			}
			value := tokens[1].text
			switch tokens[1].kind {
			case scanner.String:
				var err error
				value, err = strconv.Unquote(value)
				if err != nil {
					return errors.New("go_module_unsupported")
				}
			case scanner.Ident:
			default:
				return errors.New("go_module_unsupported")
			}
			if !validGoReportPath(value) {
				return errors.New("go_module_unsupported")
			}
			module = value
		}
		for _, token := range tokens {
			switch token.text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return errors.New("go_module_unsupported")
				}
			}
		}
		return nil
	}
	for kind := lexer.Scan(); kind != scanner.EOF; kind = lexer.Scan() {
		if lexer.Line != line {
			if err := process(); err != nil {
				return "", err
			}
			tokens = tokens[:0]
			line = lexer.Line
		}
		tokens = append(tokens, moduleToken{lexer.TokenText(), kind})
	}
	if err := process(); err != nil {
		return "", err
	}
	if lexicalError || depth != 0 {
		return "", errors.New("go_module_unsupported")
	}
	if module == "" {
		return "", errors.New("go_module_unavailable")
	}
	return module, nil
}
