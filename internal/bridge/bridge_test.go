package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"graftdiscourse/internal/ap"
	"graftdiscourse/internal/discourse"
	"graftdiscourse/internal/state"
)

const testHost = "g.example"

type replyCall struct {
	series, noteURI, content, sourceURL string
}

type fakeGraft struct {
	outbox  *ap.OrderedCollection
	replies []replyCall
}

func (f *fakeGraft) Outbox(_ context.Context, _ string) (*ap.OrderedCollection, error) {
	return f.outbox, nil
}

func (f *fakeGraft) ReplyToIssue(_ context.Context, series, noteURI, content, sourceURL string) error {
	f.replies = append(f.replies, replyCall{series, noteURI, content, sourceURL})
	return nil
}

type createdPost struct {
	topicID int64
	raw     string
}

type fakeDiscourse struct {
	topics  []discourse.Topic
	posts   []discourse.Post
	created []createdPost
}

func (f *fakeDiscourse) LatestTopics(context.Context) ([]discourse.Topic, error) {
	return f.topics, nil
}

func (f *fakeDiscourse) LatestPosts(context.Context) ([]discourse.Post, error) {
	return f.posts, nil
}

func (f *fakeDiscourse) CreatePost(_ context.Context, topicID int64, raw string) (discourse.Post, error) {
	f.created = append(f.created, createdPost{topicID, raw})
	return discourse.Post{ID: 1, TopicID: topicID}, nil
}

func newTestState(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return st
}

func fluxOutbox(issueURI, issueURL string) *ap.OrderedCollection {
	return &ap.OrderedCollection{OrderedItems: []ap.Create{
		{Object: ap.Note{ID: issueURI, Content: "Issue: Fix the flux capacitor", URL: issueURL}},
		{Object: ap.Note{ID: ap.NoteURI(testHost, "fedx", 9), Content: "comment: Fixed in #12", URL: issueURL}},
	}}
}

func TestRunOnceBothDirections(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: fluxOutbox(issueURI, issueURL)}
	d := &fakeDiscourse{
		topics: []discourse.Topic{{ID: 100, Title: "Fix the flux capacitor"}},
		posts: []discourse.Post{
			{ID: 1000, TopicID: 100, PostNumber: 2, Username: "alice", Raw: "Any progress on this?"},
			{ID: 999, TopicID: 100, PostNumber: 1, Username: "bob", Raw: "Opening the issue"},
		},
	}
	b := &Bridge{
		GraftHost: testHost,
		Series:    []string{"fedx"},
		Opts:      Options{AllowTitleMatching: true},
		Discourse: d,
		Graft:     g,
		State:     newTestState(t),
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(g.replies) != 1 || g.replies[0].noteURI != issueURI || !strings.Contains(g.replies[0].content, "Any progress on this?") {
		t.Fatalf("bad replies: %+v", g.replies)
	}
	if len(d.created) != 1 || d.created[0].topicID != 100 || !strings.Contains(d.created[0].raw, "Fixed in #12") {
		t.Fatalf("bad created: %+v", d.created)
	}
}

func TestExplicitMappingWithoutTitleMatching(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: fluxOutbox(issueURI, issueURL)}
	d := &fakeDiscourse{
		topics: []discourse.Topic{{ID: 100, Title: "totally unrelated title"}},
		posts:  []discourse.Post{{ID: 1000, TopicID: 100, PostNumber: 2, Username: "alice", Raw: "ping"}},
	}
	b := &Bridge{
		GraftHost: testHost,
		Series:    []string{"fedx"},
		Opts:      Options{Explicit: map[int64]string{100: issueURI}},
		Discourse: d,
		Graft:     g,
		State:     newTestState(t),
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(g.replies) != 1 || g.replies[0].noteURI != issueURI {
		t.Fatalf("explicit mapping not honored: %+v", g.replies)
	}
}

func TestTitleMatchingDisabledByDefault(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: fluxOutbox(issueURI, issueURL)}
	d := &fakeDiscourse{
		topics: []discourse.Topic{{ID: 100, Title: "Fix the flux capacitor"}},
		posts:  []discourse.Post{{ID: 1000, TopicID: 100, PostNumber: 2, Username: "mallory", Raw: "impersonating"}},
	}
	b := &Bridge{GraftHost: testHost, Series: []string{"fedx"}, Discourse: d, Graft: g, State: newTestState(t)}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(g.replies) != 0 {
		t.Fatalf("title matching ran without opt-in: %+v", g.replies)
	}
}

func TestCategoryAllowlist(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: fluxOutbox(issueURI, issueURL)}
	d := &fakeDiscourse{
		topics: []discourse.Topic{{ID: 100, Title: "Flux", CategoryID: 99}},
		posts:  []discourse.Post{{ID: 1000, TopicID: 100, PostNumber: 2, Username: "alice", Raw: "ping"}},
	}
	b := &Bridge{
		GraftHost: testHost,
		Series:    []string{"fedx"},
		Opts: Options{
			Explicit:          map[int64]string{100: issueURI},
			AllowedCategories: map[int64]bool{5: true},
		},
		Discourse: d, Graft: g, State: newTestState(t),
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(g.replies) != 0 {
		t.Fatalf("disallowed category was forwarded: %+v", g.replies)
	}
}

func TestRunOnceIsIdempotent(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: fluxOutbox(issueURI, issueURL)}
	d := &fakeDiscourse{
		topics: []discourse.Topic{{ID: 100, Title: "Fix the flux capacitor"}},
		posts:  []discourse.Post{{ID: 1000, TopicID: 100, PostNumber: 2, Username: "alice", Raw: "ping"}},
	}
	b := &Bridge{
		GraftHost: testHost, Series: []string{"fedx"},
		Opts:      Options{AllowTitleMatching: true},
		Discourse: d, Graft: g, State: newTestState(t),
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(g.replies) != 1 || len(d.created) != 1 {
		t.Fatalf("not idempotent: replies=%d created=%d", len(g.replies), len(d.created))
	}
}

func TestReverseDropsFediverseEcho(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: &ap.OrderedCollection{OrderedItems: []ap.Create{
		{Object: ap.Note{ID: issueURI, Content: "Issue: Flux", URL: issueURL}},
		{Object: ap.Note{
			ID:      ap.NoteURI(testHost, "fedx", 9),
			Content: "comment: via Fediverse, @alice: ping",
			URL:     issueURL,
		}},
	}}}
	d := &fakeDiscourse{topics: []discourse.Topic{{ID: 100, Title: "Flux"}}}
	b := &Bridge{
		GraftHost: testHost, Series: []string{"fedx"},
		Opts:      Options{AllowTitleMatching: true},
		Discourse: d, Graft: g, State: newTestState(t),
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(d.created) != 0 {
		t.Fatalf("fediverse echo mirrored back: %+v", d.created)
	}
}

// TestForwardSkipsBotsOwnPost reproduces the duplicate-comment bug: a
// native Forgejo/Radicle comment gets mirrored into Discourse by the
// bridge's own bot account (reverseGraftToDiscourse), and on the very
// next pass that same post — authored by the bot, not a real user —
// must not be picked back up and forwarded to Graft as if it were a
// fresh reply. Without BotUsername set, this failed: the bot's own post
// (post_number 2, no "via Fediverse" marker to catch it) sailed through.
func TestForwardSkipsBotsOwnPost(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"

	g := &fakeGraft{outbox: fluxOutbox(issueURI, issueURL)}
	d := &fakeDiscourse{
		topics: []discourse.Topic{{ID: 100, Title: "Fix the flux capacitor"}},
		posts: []discourse.Post{
			{ID: 1000, TopicID: 100, PostNumber: 2, Username: "bridge-bot", Raw: "**Comment on the repository:**\n\nFixed in #12"},
		},
	}
	b := &Bridge{
		GraftHost: testHost,
		Series:    []string{"fedx"},
		Opts:      Options{AllowTitleMatching: true, BotUsername: "bridge-bot"},
		Discourse: d,
		Graft:     g,
		State:     newTestState(t),
	}
	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(g.replies) != 0 {
		t.Fatalf("bot's own post was forwarded back to Graft: %+v", g.replies)
	}
}

func TestBindValidatesNote(t *testing.T) {
	issueURI := ap.NoteURI(testHost, "fedx", 7)
	issueURL := "https://forgejo.example/fedx/issues/7"
	g := &fakeGraft{outbox: &ap.OrderedCollection{OrderedItems: []ap.Create{
		{Object: ap.Note{ID: issueURI, Content: "Issue: Flux", URL: issueURL}},
		{Object: ap.Note{ID: ap.NoteURI(testHost, "fedx", 8), Content: "Commit: bump"}},
	}}}
	b := &Bridge{GraftHost: testHost, Series: []string{"fedx"}, Graft: g, State: newTestState(t)}
	ctx := context.Background()

	if err := b.Bind(ctx, 100, issueURI); err != nil {
		t.Fatalf("bind issue: %v", err)
	}
	if uri, ok := b.State.TopicNote(100); !ok || uri != issueURI {
		t.Fatalf("mapping not stored: %q %v", uri, ok)
	}
	if err := b.Bind(ctx, 101, ap.NoteURI(testHost, "fedx", 8)); err == nil {
		t.Fatal("bind to a commit note should fail")
	}
	if err := b.Bind(ctx, 102, "https://evil.example/actors/fedx/notes/7"); err == nil {
		t.Fatal("bind to a foreign host should fail")
	}
	if err := b.Bind(ctx, 103, ap.NoteURI(testHost, "other", 1)); err == nil {
		t.Fatal("bind to a non-configured series should fail")
	}
	if err := b.Unbind(100); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if _, ok := b.State.TopicNote(100); ok {
		t.Fatal("mapping not removed")
	}
}

func TestPostURL(t *testing.T) {
	cases := []struct {
		web, slug string
		topic     int64
		num       int
		want      string
	}{
		{"https://discourse.example/", "my-topic", 12, 3, "https://discourse.example/t/my-topic/12/3"},
		{"https://discourse.example", "", 12, 3, "https://discourse.example/t/12/3"},
		{"", "my-topic", 12, 3, ""},
	}
	for _, c := range cases {
		if got := postURL(c.web, c.slug, c.topic, c.num); got != c.want {
			t.Errorf("postURL(%q,%q,%d,%d) = %q, want %q", c.web, c.slug, c.topic, c.num, got, c.want)
		}
	}
}
