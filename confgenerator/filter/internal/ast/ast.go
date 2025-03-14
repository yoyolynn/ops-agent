// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ast

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/filter/internal/generated/token"
	"github.com/GoogleCloudPlatform/ops-agent/confgenerator/fluentbit"
)

type Attrib interface{}

// Cloud Logging's filter syntax has extremely weird rules around quoting/escaping
//
// A "text" is a literal that is not surrounded by double quotes. A
// text may contain a small number of special characters, must start
// with a non-digit, and can contain some but not all escape
// sequences. However, if it does contain escape sequences, they are
// NOT unescaped before matching.
//
// A "string" is a literal that IS surrounded by double quotes. A
// string can contain a larger number of special characters and escape
// sequences, and the escape sequences ARE unescaped. However, if it
// is used for a regex, the escape sequences are unescaped by the
// regex engine, not separately as part of filter parsing.

// Target represents member from the filter BNF, and represents either a value or a dotted field path.
// Each element of the slice is not yet unescaped (if needed).
type Target []string

var logEntryRootValueMapToFluentBit = map[string]string{
	"severity": "logging.googleapis.com/severity",
	"logName":  "logging.googleapis.com/logName",
}

var logEntryRootStructMapToFluentBit = map[string]string{
	"labels":         "logging.googleapis.com/labels",
	"operation":      "logging.googleapis.com/operation",
	"sourceLocation": "logging.googleapis.com/sourceLocation",
	"httpRequest":    "logging.googleapis.com/http_request",
}

func (m Target) fluentBitPath() ([]string, error) {
	unquoted, err := m.Unquote()
	if err != nil {
		return nil, err
	}
	var fluentBit []string
	if len(unquoted) == 1 {
		if v, ok := logEntryRootValueMapToFluentBit[unquoted[0]]; ok {
			fluentBit = []string{v}
		}
	} else if len(m) > 1 {
		if v, ok := logEntryRootStructMapToFluentBit[unquoted[0]]; ok {
			fluentBit = prepend(v, unquoted[1:])
		} else if unquoted[0] == "jsonPayload" {
			// Special case for jsonPayload, where the root "jsonPayload" must be omitted
			fluentBit = unquoted[1:]
		}
	}
	if fluentBit == nil {
		return nil, fmt.Errorf("field %q not found", strings.Join(m, "."))
	}
	return fluentBit, nil
}

// Equals checks if two valid targets are equal.
// Invalid targets are never equal.
func (m Target) Equals(m2 Target) bool {
	s1, err := m.fluentBitPath()
	if err != nil {
		return false
	}
	s2, err := m2.fluentBitPath()
	if err != nil {
		return false
	}
	if len(s1) != len(s2) {
		return false
	}
	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}
	return true
}

// RecordAccessor returns a string that can be used as a key in a FluentBit config
func (m Target) RecordAccessor() (string, error) {
	fluentBit, err := m.fluentBitPath()
	if err != nil {
		return "", err
	}
	recordAccessor := "$record"
	for _, part := range fluentBit {
		// Disallowed characters because they cannot be encoded in a Record Accessor.
		// \r is allowed in a Record Accessor, but we disallow it to avoid issues on Windows.
		// (interestingly, \f and \v work fine...)
		if strings.ContainsAny(part, "\n\r\", ") {
			return "", fmt.Errorf("target may not contain line breaks, spaces, commas, or double-quotes: %q", part)
		}
		recordAccessor = recordAccessor + fmt.Sprintf(`['%s']`, strings.ReplaceAll(part, `'`, `''`))
	}
	return recordAccessor, nil
}

// LuaAccessor returns the value of the target (with write=false) or a function that takes one argument to set the target (with write=true).
func (m Target) LuaAccessor(write bool) (string, error) {
	fluentBit, err := m.fluentBitPath()
	if err != nil {
		return "", err
	}
	// Make a copy so we don't mutate m
	fluentBit = append([]string{}, fluentBit...)
	for i := range fluentBit {
		fluentBit[i] = LuaQuote(fluentBit[i])
	}
	var out strings.Builder
	if write {
		out.WriteString(`(function(value)
`)
	} else {
		out.WriteString(`(function()
`)
	}
	for i := 0; i < len(fluentBit)-1; i++ {
		p := strings.Join(fluentBit[:i+1], "][")
		fmt.Fprintf(&out, `if record[%s] == nil
then
`, p)
		if write {
			fmt.Fprintf(&out, `record[%s] = {}
`, p)
		} else {
			fmt.Fprintf(&out, `return nil`)
		}
		fmt.Fprintf(&out, "end\n")
	}
	p := strings.Join(fluentBit, "][")
	if write {
		fmt.Fprintf(&out, `record[%s] = value
end)`, p)
	} else {
		fmt.Fprintf(&out, `return record[%s]
end)()`, p)
	}
	return out.String(), nil
}

const (
	filterStartChar  = `#$%&'*/;?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[]^_` + "`" + `abcdefghijklmnopqrstuvwxyz{|}`
	filterMidChar    = filterStartChar + `0123456789+-`
	filterStringChar = filterMidChar + `!(),.:<=>~`
)

func escapeFilterString(in string) string {
	var needQuotes bool
	var b strings.Builder
	for i, c := range in {
		if i == 0 {
			if strings.ContainsRune(filterStartChar, c) {
				b.WriteRune(c)
				continue
			}
			needQuotes = true
		}
		if strings.ContainsRune(filterMidChar, c) {
			b.WriteRune(c)
			continue
		}
		needQuotes = true
		if strings.ContainsRune(filterStringChar, c) {
			b.WriteRune(c)
		} else if c == '\a' {
			b.WriteString(`\a`)
		} else if c == '\b' {
			b.WriteString(`\b`)
		} else if c == '\f' {
			b.WriteString(`\f`)
		} else if c == '\n' {
			b.WriteString(`\n`)
		} else if c == '\r' {
			b.WriteString(`\r`)
		} else if c == '\t' {
			b.WriteString(`\t`)
		} else if c == '\v' {
			b.WriteString(`\v`)
		} else {
			fmt.Fprintf(&b, `\u%04X`, c)
		}
	}
	if needQuotes {
		return fmt.Sprintf(`"%s"`, b.String())
	}
	return b.String()
}

func (m Target) Unquote() ([]string, error) {
	var unquoted []string
	for _, part := range m {
		p, err := UnquoteTextOrString(part)
		if err != nil {
			return nil, err
		}
		unquoted = append(unquoted, p)
	}
	return unquoted, nil
}

// String formats a target as a valid expression
func (m Target) String() string {
	var out []string
	unquoted, err := m.Unquote()
	if err != nil {
		return fmt.Sprintf("UNPARSABLE TARGET %#v", m)
	}
	for _, s := range unquoted {
		out = append(out, escapeFilterString(s))
	}
	return strings.Join(out, ".")
}

func prepend(value string, slice []string) []string {
	return append([]string{value}, slice...)
}

type Restriction struct {
	Operator string
	// LHS contains the field being matched
	LHS Target
	// RHS contains the string to match against; for regexes, this is a raw string including escape sequences (but always without double quotes).
	RHS string
}

func NewRestriction(lhs, operator, rhs Attrib) (*Restriction, error) {
	var r Restriction
	switch operator := operator.(type) {
	case string:
		r.Operator = operator
	case *token.Token:
		r.Operator = string(operator.Lit)
	default:
		return nil, fmt.Errorf("unknown operator: %v", operator)
	}
	switch lhs := lhs.(type) {
	case Target:
		// Eager validation
		_, err := lhs.RecordAccessor()
		if err != nil {
			return nil, err
		}
		r.LHS = lhs
	default:
		return nil, fmt.Errorf("unknown lhs: %v", lhs)
	}
	switch rhs := rhs.(type) {
	case nil:
	case Target:
		// BNF parses values as Target, even if they are singular
		if len(rhs) != 1 {
			return nil, fmt.Errorf("unexpected rhs: %v", rhs)
		}
		// Perform the appropriate unquoting depending on what operator is being used.
		switch r.Operator {
		case "=~", "!~":
			rhs := rhs[0]
			// Regular expressions must be string, not text, and we need to preserve the original escaped text for the regex engine.
			if rhs[0] != byte('"') || rhs[len(rhs)-1] != byte('"') {
				return nil, fmt.Errorf("regular expressions must begin and end with '\"', token %q", rhs)
			}
			r.RHS = rhs[1 : len(rhs)-1]
		default:
			rhs, err := UnquoteTextOrString(rhs[0])
			if err != nil {
				return nil, err
			}
			r.RHS = rhs
		}
	default:
		return nil, fmt.Errorf("unknown rhs: %v", rhs)
	}
	return &r, nil
}

func (r Restriction) Simplify() Expression {
	return r
}

func (r Restriction) String() string {
	if r.Operator == "GLOBAL" {
		return escapeFilterString(r.RHS)
	}
	switch r.Operator {
	case "=~", "!~":
		return fmt.Sprintf(`%s %s "%s"`, r.LHS, r.Operator, r.RHS)
	}
	return fmt.Sprintf(`%s %s %s`, r.LHS, r.Operator, escapeFilterString(r.RHS))
}

func modify(tag, key string) fluentbit.Component {
	return fluentbit.Component{
		Kind: "FILTER",
		Config: map[string]string{
			"Name":  "modify",
			"Match": tag,
			"Set":   fmt.Sprintf("%s 1", key),
		},
	}
}

func cond(ctype string, values ...string) string {
	return fmt.Sprintf("%s %s", ctype, strings.Join(values, " "))
}

func escapeWhitespaceFluentBit(s string) string {
	s = strings.ReplaceAll(s, "\a", `\a`)
	s = strings.ReplaceAll(s, "\b", `\x08`)
	s = strings.ReplaceAll(s, "\f", `\f`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	s = strings.ReplaceAll(s, "\v", `\v`)
	s = strings.ReplaceAll(s, " ", `\x20`)
	return s
}

func (r Restriction) FluentConfig(tag, key string) ([]fluentbit.Component, string) {
	lhs, _ := r.LHS.LuaAccessor(false)
	rhsQuoted := LuaQuote(r.RHS)
	rhsRegex := escapeWhitespaceFluentBit(r.RHS)

	// TODO: Add support for numeric comparisons

	switch r.Operator {
	case "GLOBAL", "<", "<=", ">", ">=":
		panic(fmt.Errorf("unimplemented operator: %s", r.Operator))
	case ":":
		// substring match, case insensitive
		return nil, fmt.Sprintf(`(string.find(string.lower(%s), string.lower(%s), 1, false) != nil)`, lhs, rhsQuoted)
	case "=~", "!~":
		// regex match, case sensitive

		// TODO: Re-implement using Lua once regex is supported. Lua has been shown to perform better
		// than the next/modify/lift pattern used here, but we are unable to use Lua for now since
		// it does not yet support regex.

		c := modify(tag, key)
		lhsRA, err := r.LHS.RecordAccessor()
		if err != nil {
			panic(fmt.Errorf("LHS %v couldn't parse: %w", r.LHS, err))
		}
		if r.Operator == "=~" {
			c.Config["Condition"] = cond("Key_value_matches", lhsRA, rhsRegex)
		} else {
			c.OrderedConfig = append(c.OrderedConfig, [2]string{"Condition", cond("Key_value_does_not_match", lhsRA, rhsRegex)})
		}
		return []fluentbit.Component{c}, fmt.Sprintf(`(record[%s] ~= nil)`, LuaQuote(key))
	case "=":
		// equality, case insensitive
		return nil, fmt.Sprintf(`(string.lower(%s) == string.lower(%s))`, lhs, rhsQuoted)
	case "!=":
		// inequality, case insensitive
		return nil, fmt.Sprintf(`(string.lower(%s) ~= string.lower(%s))`, lhs, rhsQuoted)
	}
	// This is all the supported operators.
	panic(fmt.Errorf("unknown operator: %s", r.Operator))
}

type Expression interface {
	// Simplify returns a logically equivalent Expression.
	Simplify() Expression

	// FluentConfig returns an optional sequence of fluentbit operations and a Lua expression that can be evaluated to determine if the expression matches the record.
	FluentConfig(tag, key string) ([]fluentbit.Component, string)

	fmt.Stringer
}

func Simplify(a Attrib) (Expression, error) {
	switch a := a.(type) {
	case Expression:
		return a.Simplify(), nil
	}
	return nil, fmt.Errorf("expected expression: %v", a)
}

// Conjunction represents an AND expression
type Conjunction []Expression

// Disjunction represents an OR expression
type Disjunction []Expression

func NewConjunction(a Attrib) (Conjunction, error) {
	switch a := a.(type) {
	case Conjunction:
		return a, nil
	case Expression:
		return Conjunction{a.Simplify()}, nil
	}
	return nil, fmt.Errorf("expected expression: %v", a)
}

func (c Conjunction) Simplify() Expression {
	if len(c) == 1 {
		return c[0]
	}
	return c
}

func (c Conjunction) Append(a Attrib) (Conjunction, error) {
	switch a := a.(type) {
	case Conjunction:
		return append(c, a...), nil
	case Expression:
		return append(c, a.Simplify()), nil
	}
	return nil, fmt.Errorf("expected expression: %v", a)
}

type exprSlice []Expression

func (s exprSlice) FluentConfig(tag, key, operator string) ([]fluentbit.Component, string) {
	var components []fluentbit.Component
	var exprs []string
	for i, e := range s {
		subkey := fmt.Sprintf("%s_%d", key, i)
		exprComponents, expr := e.FluentConfig(tag, subkey)
		components = append(components, exprComponents...)
		exprs = append(exprs, expr)
	}
	return components, fmt.Sprintf(`(%s)`, strings.Join(exprs, operator))
}

func (s exprSlice) String(operator string) string {
	var out []string
	for _, e := range s {
		out = append(out, e.String())
	}
	return fmt.Sprintf("(%s)", strings.Join(out, ") "+operator+" ("))
}

func (c Conjunction) FluentConfig(tag, key string) ([]fluentbit.Component, string) {
	return exprSlice(c).FluentConfig(tag, key, " and ")
}

func (c Conjunction) String() string {
	return exprSlice(c).String("AND")
}

func NewDisjunction(a Attrib) (Disjunction, error) {
	switch a := a.(type) {
	case Disjunction:
		return a, nil
	case Expression:
		return Disjunction{a.Simplify()}, nil
	}
	return nil, fmt.Errorf("expected expression: %v", a)
}

func (d Disjunction) Simplify() Expression {
	if len(d) == 1 {
		return d[0]
	}
	return d
}

func (d Disjunction) Append(a Attrib) (Disjunction, error) {
	switch a := a.(type) {
	case Disjunction:
		return append(d, a...), nil
	case Expression:
		return append(d, a.Simplify()), nil
	}
	return nil, fmt.Errorf("expected expression: %v", a)
}

func (d Disjunction) FluentConfig(tag, key string) ([]fluentbit.Component, string) {
	return exprSlice(d).FluentConfig(tag, key, " or ")
}

func (d Disjunction) String() string {
	return exprSlice(d).String("OR")
}

type Negation struct {
	Expression
}

func (n Negation) Simplify() Expression {
	return Negation{n.Expression.Simplify()}
}

func (n Negation) FluentConfig(tag, key string) ([]fluentbit.Component, string) {
	c, expr := n.Expression.FluentConfig(tag, key)
	return c, fmt.Sprintf("(not %s)", expr)
}

func (n Negation) String() string {
	return fmt.Sprintf("NOT %s", n.Expression.String())
}

// UnquoteTextOrString returns text literals as-is and unquotes string literals.
func UnquoteTextOrString(in string) (string, error) {
	if in[0] == byte('"') {
		return UnquoteString(in[1 : len(in)-1])
	}
	return in, nil
}

// UnquoteString replaces all escape sequences with their respective characters that they represent.
//
// It assumes the leading and trailing double-quotes have been removed.
//
// Escape sequences are replaced if and only if they are defined in our grammar: confgenerator/filter/internal/filter.bnf.
// An error is returned if an unrecognized escape sequence is encountered.
//
// This is a compatibility layer to maintain parity with Cloud Logging query strings. strconv.Unquote cannot be used here
// because it follows escape rules for Go strings, and Cloud Logging strings are not Go strings.
func UnquoteString(in string) (string, error) {
	var buf strings.Builder
	buf.Grow(3 * len(in) / 2)

	r := strings.NewReader(in)
	for {
		c, _, err := r.ReadRune()
		if err != nil {
			// EOF is only possible error
			break
		}
		if c != '\\' {
			buf.WriteRune(c)
			continue
		}
		c, _, err = r.ReadRune()
		if err != nil {
			buf.WriteRune('\\')
			break
		}
		switch c {
		case ',', ':', '=', '<', '>', '+', '~', '"', '\\', '.', '*':
			// escaped_char
			buf.WriteRune(c)
		case 'u':
			// unicode_esc
			// [0-9a-f]{4}
			digits := make([]byte, 4)
			n, _ := r.Read(digits)
			digits = digits[:n]
			codepoint, err := strconv.ParseUint(string(digits), 16, 16)
			if n < 4 || err != nil {
				buf.WriteRune('\\')
				buf.WriteRune('u')
				buf.Write(digits)
				break
			}
			buf.WriteRune(rune(codepoint))
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// octal_esc
			// [0-7]{1,2} or [0-3],[0-7]{2}
			digits := []byte{byte(c)}
			for len(digits) < 3 {
				c, err := r.ReadByte()
				if err != nil {
					break
				}
				if c < '0' || c > '7' {
					r.UnreadByte()
					break
				}
				digits = append(digits, c)
				if digits[0] > '3' && len(digits) == 2 {
					break
				}
			}
			codepoint, err := strconv.ParseUint(string(digits), 8, 8)
			if err != nil {
				buf.WriteRune('\\')
				buf.Write(digits)
				break
			}
			buf.WriteRune(rune(codepoint))
		case 'x':
			// hex_esc:
			// 2*hex_digit
			digits := make([]byte, 2)
			n, _ := r.Read(digits)
			digits = digits[:n]
			codepoint, err := strconv.ParseUint(string(digits), 16, 8)
			if n < 2 || err != nil {
				buf.WriteRune('\\')
				buf.WriteRune('x')
				buf.Write(digits)
				break
			}
			buf.WriteRune(rune(codepoint))
		case 'a':
			buf.WriteRune('\a')
		case 'b':
			buf.WriteRune('\b')
		case 'f':
			buf.WriteRune('\f')
		case 'n':
			buf.WriteRune('\n')
		case 'r':
			buf.WriteRune('\r')
		case 't':
			buf.WriteRune('\t')
		case 'v':
			buf.WriteRune('\v')
		default:
			return "", fmt.Errorf(`invalid escape sequence: \%s`, string(c))
		}
	}
	return buf.String(), nil
}

func ParseTextOrString(a Attrib) (string, error) {
	str := string(a.(*token.Token).Lit)
	return str, nil
}

func LuaQuote(in string) string {
	var b strings.Builder
	b.Grow(len(in) + 2)
	b.WriteString(`"`)
	for i := 0; i < len(in); i++ {
		// N.B. slicing a string gives bytes, not runes
		c := in[i]
		if c == 92 {
			b.WriteString(`\\`)
		} else if c == 34 {
			b.WriteString(`\"`)
		} else if c >= 32 && c < 127 {
			// printable character
			b.WriteByte(c)
		} else {
			// N.B. Lua character escapes are always integers
			fmt.Fprintf(&b, `\%d`, c)
		}
	}
	b.WriteString(`"`)
	return b.String()
}
