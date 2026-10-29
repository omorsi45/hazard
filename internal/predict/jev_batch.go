package predict

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/redact"
)

const (
	// maxDescriptorTokens caps one test descriptor.
	maxDescriptorTokens = 2000
	// minDescriptorBytes is the smallest descriptor worth sending: below it a
	// request cannot carry the test at all.
	minDescriptorBytes = 32
)

// jevRequest is the body of a Decisions request. The field order is the
// serialization order.
type jevRequest struct {
	Model     string                 `json:"model"`
	State     jevState               `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevState struct {
	Rubric string                 `json:"rubric"`
	Change redact.Change          `json:"change"`
	Tests  map[string]redact.Test `json:"tests"`
}

type jevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// estimateTokens is ceil(n / 3.5), the conservative token estimate of n bytes
// of serialized JSON.
func estimateTokens(n int) int { return (2*n + 6) / 7 }

// bytesForTokens is the largest byte count whose estimate is at most tokens.
func bytesForTokens(tokens int) int { return tokens * 7 / 2 }

// canonicalJSON serializes v compactly, with struct fields in declaration
// order, map keys sorted and no HTML escaping. It is the form that is sent
// and the form that is hashed.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// questionKey names the n-th test of a shard, counted from 1.
func questionKey(n int) string { return fmt.Sprintf("t_%03d", n) }

// descriptor is one candidate as it is sent.
type descriptor struct {
	index     int // position in the caller's candidate slice
	testID    string
	test      redact.Test
	body      []byte // canonical JSON of test
	hash      string // sha256 of body
	truncated bool
}

// truncate cuts the descriptor to at most maxBytes of JSON.
func (d *descriptor) truncate(maxBytes int) error {
	t, err := truncateTest(d.test, maxBytes)
	if err != nil {
		return fmt.Errorf("test %s: %w", d.testID, err)
	}
	body, err := canonicalJSON(t)
	if err != nil {
		return err
	}
	d.test, d.body, d.hash, d.truncated = t, body, hashHex(body), true
	return nil
}

func jsonLen(v any) (int, error) {
	b, err := canonicalJSON(v)
	return len(b), err
}

// truncateTest shortens t until its JSON is at most maxBytes: trailing
// assertions go first, then the purpose, the name and the package are cut.
func truncateTest(t redact.Test, maxBytes int) (redact.Test, error) {
	n, err := jsonLen(t)
	if err != nil {
		return t, err
	}
	asserts := t.Asserts
	for n > maxBytes && len(asserts) > 0 {
		asserts = asserts[:len(asserts)-1]
		t.Asserts = asserts
		if n, err = jsonLen(t); err != nil {
			return t, err
		}
	}
	for _, field := range []*string{&t.Purpose, &t.Name, &t.Package} {
		if n <= maxBytes {
			break
		}
		// Every raw byte is at least one byte of JSON, so cutting the excess
		// from the raw text removes at least the excess from the JSON.
		cut := min(n-maxBytes, len(*field))
		*field = cutUTF8(*field, len(*field)-cut)
		if n, err = jsonLen(t); err != nil {
			return t, err
		}
	}
	if n > maxBytes {
		return t, fmt.Errorf("a descriptor needs at least %d bytes, over the limit of %d", n, maxBytes)
	}
	return t, nil
}

// cutUTF8 returns the first n bytes of s, shortened to a rune boundary.
func cutUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// batch is everything a call needs to ask about its candidates: the change
// block, the descriptors and the sizes of an empty request.
type batch struct {
	j           *Jev
	block       redact.Change
	fingerprint string
	descs       []*descriptor // in the caller's order
	baseState   int           // state with no test
	baseRequest int           // request with no test and no question
	tierEntry   int           // bytes of the tier question entry
	questions   map[int]int   // bytes of the question entry at each position
}

// prepare builds the change block and the descriptors of cands.
func (j *Jev) prepare(ch *change.ChangeSet, cands []impact.Candidate) (*batch, error) {
	if ch == nil {
		return nil, errors.New("jev: no change set")
	}
	block := redact.BuildChange(ch, j.privacy)
	// The commit ids carry no information about the change. Leaving them out
	// of the fingerprint lets a rebase or a second branch with the same diff
	// hit the cache.
	anonymous := block
	anonymous.Base, anonymous.Head = "", ""
	body, err := canonicalJSON(anonymous)
	if err != nil {
		return nil, err
	}
	b := &batch{j: j, block: block, fingerprint: hashHex(body), questions: map[int]int{}}
	for i, c := range cands {
		t := redact.BuildTest(c, j.privacy)
		tb, err := canonicalJSON(t)
		if err != nil {
			return nil, err
		}
		d := &descriptor{index: i, testID: c.TestID, test: t, body: tb, hash: hashHex(tb)}
		if estimateTokens(len(tb)) > maxDescriptorTokens {
			if err := d.truncate(bytesForTokens(maxDescriptorTokens)); err != nil {
				return nil, err
			}
		}
		b.descs = append(b.descs, d)
	}
	skeleton := b.request(nil)
	if b.baseRequest, err = jsonLen(skeleton); err != nil {
		return nil, err
	}
	if b.baseState, err = jsonLen(skeleton.State); err != nil {
		return nil, err
	}
	tq, err := canonicalJSON(tierQuestion())
	if err != nil {
		return nil, err
	}
	b.tierEntry = len(tierKey) + 3 + len(tq)
	return b, nil
}

func tierQuestion() jevQuestion {
	return jevQuestion{Type: "choice", Instructions: tierInstructions, Criteria: tierCriteria}
}

func (b *batch) question(key string) jevQuestion {
	return jevQuestion{Type: "noul", Instructions: b.j.rubric.instructions(key), Criteria: questionCriteria}
}

// questionEntry is the byte length of the question entry at position pos.
func (b *batch) questionEntry(pos int) (int, error) {
	if n, ok := b.questions[pos]; ok {
		return n, nil
	}
	key := questionKey(pos)
	body, err := canonicalJSON(b.question(key))
	if err != nil {
		return 0, err
	}
	n := len(key) + 3 + len(body)
	b.questions[pos] = n
	return n, nil
}

// request assembles the request of one shard. A nil shard gives the empty
// skeleton.
func (b *batch) request(s *shard) jevRequest {
	req := jevRequest{
		Model: b.j.cfg.Model,
		State: jevState{
			Rubric: rubricText, Change: b.block,
			Tests: map[string]redact.Test{},
		},
		Questions: map[string]jevQuestion{},
	}
	if s == nil {
		return req
	}
	for i, d := range s.members {
		key := s.keys[i]
		req.State.Tests[key] = d.test
		req.Questions[key] = b.question(key)
	}
	if s.tier {
		req.Questions[tierKey] = tierQuestion()
	}
	return req
}

// shard is one request: the tests it asks about and its serialized body.
type shard struct {
	index   int
	members []*descriptor // in TestID order
	keys    []string
	tier    bool
	body    []byte

	testBytes     int // bytes of the test entries
	questionBytes int // bytes of the question entries
	questions     int // number of questions, the tier question included
}

func (b *batch) newShard(index int) *shard {
	s := &shard{index: index}
	if b.j.askTier && index == 0 {
		s.tier = true
		s.questionBytes = b.tierEntry
		s.questions = 1
	}
	return s
}

// fits reports whether a test entry of te bytes and a question entry of qe
// bytes can join s within the question count and both token limits.
func (b *batch) fits(s *shard, te, qe int) bool {
	cfg := b.j.cfg
	if s.questions+1 > cfg.QuestionsPerRequest {
		return false
	}
	n := len(s.members)
	if estimateTokens(b.baseState+s.testBytes+te+n) > cfg.MaxStateTokens {
		return false
	}
	return estimateTokens(b.baseRequest+s.testBytes+te+n+s.questionBytes+qe+s.questions) <= cfg.MaxRequestTokens
}

// checkBase fails when the request without any test already exceeds a limit,
// which no amount of sharding can fix.
func (b *batch) checkBase(s *shard) error {
	cfg := b.j.cfg
	if t := estimateTokens(b.baseState); t > cfg.MaxStateTokens {
		return fmt.Errorf("the change block does not fit: the state estimates %d tokens, max_state_tokens is %d", t, cfg.MaxStateTokens)
	}
	if t := estimateTokens(b.baseRequest + s.questionBytes); t > cfg.MaxRequestTokens {
		return fmt.Errorf("the change block does not fit: the request estimates %d tokens, max_request_tokens is %d", t, cfg.MaxRequestTokens)
	}
	return nil
}

// shards splits ds into requests, greedily in TestID order. A descriptor that
// does not fit even in an empty request is cut down until it does.
func (b *batch) shards(ds []*descriptor) ([]*shard, error) {
	if len(ds) == 0 {
		return nil, nil
	}
	ordered := append([]*descriptor(nil), ds...)
	sort.SliceStable(ordered, func(i, k int) bool { return ordered[i].testID < ordered[k].testID })

	cur := b.newShard(0)
	if err := b.checkBase(cur); err != nil {
		return nil, err
	}
	var out []*shard
	for _, d := range ordered {
		for {
			pos := len(cur.members) + 1
			key := questionKey(pos)
			te := len(key) + 3 + len(d.body)
			qe, err := b.questionEntry(pos)
			if err != nil {
				return nil, err
			}
			if b.fits(cur, te, qe) {
				cur.members = append(cur.members, d)
				cur.keys = append(cur.keys, key)
				cur.testBytes += te
				cur.questionBytes += qe
				cur.questions++
				break
			}
			if len(cur.members) > 0 {
				out = append(out, cur)
				cur = b.newShard(len(out))
				continue
			}
			if err := b.shrink(cur, d, te, qe); err != nil {
				return nil, err
			}
		}
	}
	out = append(out, cur)
	for _, s := range out {
		if err := b.seal(s); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// shrink cuts d, which does not fit in the empty shard s, by the number of
// bytes it exceeds the limits by.
func (b *batch) shrink(s *shard, d *descriptor, te, qe int) error {
	cfg := b.j.cfg
	over := max(
		b.baseState+te-bytesForTokens(cfg.MaxStateTokens),
		b.baseRequest+te+s.questionBytes+qe+s.questions-bytesForTokens(cfg.MaxRequestTokens),
	)
	target := len(d.body) - over
	if over <= 0 || target < minDescriptorBytes {
		return fmt.Errorf("test %s cannot be made to fit a request (max_state_tokens %d, max_request_tokens %d)", d.testID, cfg.MaxStateTokens, cfg.MaxRequestTokens)
	}
	return d.truncate(target)
}

// seal serializes s and checks the real size against the limits.
func (b *batch) seal(s *shard) error {
	req := b.request(s)
	body, err := canonicalJSON(req)
	if err != nil {
		return err
	}
	state, err := jsonLen(req.State)
	if err != nil {
		return err
	}
	cfg := b.j.cfg
	if t := estimateTokens(len(body)); t > cfg.MaxRequestTokens {
		return fmt.Errorf("request %d estimates %d tokens, max_request_tokens is %d", s.index, t, cfg.MaxRequestTokens)
	}
	if t := estimateTokens(state); t > cfg.MaxStateTokens {
		return fmt.Errorf("request %d state estimates %d tokens, max_state_tokens is %d", s.index, t, cfg.MaxStateTokens)
	}
	s.body = body
	return nil
}

// Payload returns the exact bytes of every request a call would send if no
// answer came from the cache. It makes no network call.
func (j *Jev) Payload(ch *change.ChangeSet, cands []impact.Candidate) ([][]byte, error) {
	b, err := j.prepare(ch, cands)
	if err != nil {
		return nil, err
	}
	shards, err := b.shards(b.descs)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(shards))
	for i, s := range shards {
		out[i] = s.body
	}
	return out, nil
}
