package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"graftdiscourse/internal/ap"
	"graftdiscourse/internal/discourse"
	"graftdiscourse/internal/graft"
	"graftdiscourse/internal/state"
)

// discourseAPI is the subset of the Discourse client the bridge needs.
type discourseAPI interface {
	LatestTopics(ctx context.Context) ([]discourse.Topic, error)
	LatestPosts(ctx context.Context) ([]discourse.Post, error)
	CreatePost(ctx context.Context, topicID int64, raw string) (discourse.Post, error)
}

// graftAPI is the subset of the Graft client the bridge needs.
type graftAPI interface {
	Outbox(ctx context.Context, series string) (*ap.OrderedCollection, error)
	ReplyToIssue(ctx context.Context, series, noteURI, content, sourceURL string) error
}

// Options are the security-relevant knobs of a bridge pass.
type Options struct {
	// Explicit maps a Discourse topic id to a Graft note URI. This is the
	// trusted way to bind a topic to a repo issue.
	Explicit map[int64]string
	// AllowTitleMatching enables the heuristic topic-title == issue-title
	// fallback. Off by default.
	AllowTitleMatching bool
	// AllowedCategories, when non-empty, restricts forwarding to these
	// Discourse category ids.
	AllowedCategories map[int64]bool
	// MaxContentRunes caps a single mirrored message.
	MaxContentRunes int
	// MaxDeliveriesPerPass bounds outbound comments per pass.
	MaxDeliveriesPerPass int
	// BotUsername is the Discourse account the bridge itself posts as
	// (discourse.api_username). A post authored by this account is
	// always the bridge's own reverse-mirror (see reverseGraftToDiscourse)
	// — forwarding it back to Graft would create a duplicate comment
	// every single time a native Forgejo/Radicle comment gets mirrored
	// out, since Graft's own outbound "via Fediverse" echo guard only
	// catches the *second* lap, not this first spurious one.
	BotUsername string
}

// Bridge reconciles Discourse topics and Graft-mirrored repo issues.
type Bridge struct {
	GraftHost string
	Series    []string
	// MaxPosts bounds how many of the most recent Discourse posts each pass
	// inspects (0 = no bound).
	MaxPosts int
	Opts     Options

	Discourse discourseAPI
	Graft     graftAPI
	State     *state.State
	Log       *slog.Logger
}

// RunOnce performs one full reconciliation pass.
func (b *Bridge) RunOnce(ctx context.Context) error {
	topics, err := b.Discourse.LatestTopics(ctx)
	if err != nil {
		return fmt.Errorf("discourse latest topics: %w", err)
	}
	notes, err := b.refreshMapping(ctx, topics)
	if err != nil {
		return err
	}
	if err := b.forwardDiscourseToGraft(ctx, topics); err != nil {
		return err
	}
	return b.reverseGraftToDiscourse(ctx, notes)
}

// Issues lists the replyable issue/patch notes across the configured
// series, so operators can choose a note URI to bind.
func (b *Bridge) Issues(ctx context.Context) ([]graft.IssueRef, error) {
	var out []graft.IssueRef
	for _, series := range b.Series {
		oc, err := b.Graft.Outbox(ctx, series)
		if err != nil {
			return out, fmt.Errorf("fetch %s outbox: %w", series, err)
		}
		for _, n := range graft.ParseNotes(b.GraftHost, oc) {
			if !n.IsIssueOrPatch() {
				continue
			}
			out = append(out, graft.IssueRef{
				Series:  series,
				EntryID: n.EntryID,
				Kind:    n.Kind,
				Title:   n.Summary,
				NoteURI: n.URI,
				URL:     n.URL,
			})
		}
	}
	return out, nil
}

// Bind links a Discourse topic to a Graft note, validating that the note
// belongs to a configured series and is an issue/patch Graft will accept
// replies to. It persists the mapping and returns an error otherwise.
func (b *Bridge) Bind(ctx context.Context, topicID int64, noteURI string) error {
	series, _, ok := ap.ParseNoteURI(b.GraftHost, noteURI)
	if !ok {
		return fmt.Errorf("note_uri %q does not belong to %s", noteURI, b.GraftHost)
	}
	if !b.seriesConfigured(series) {
		return fmt.Errorf("series %q is not configured", series)
	}
	oc, err := b.Graft.Outbox(ctx, series)
	if err != nil {
		return fmt.Errorf("fetch %s outbox: %w", series, err)
	}
	for _, n := range graft.ParseNotes(b.GraftHost, oc) {
		if n.URI != noteURI {
			continue
		}
		if !n.IsIssueOrPatch() {
			return fmt.Errorf("note is a %s; Graft only accepts replies to issues and patches", n.Kind)
		}
		if err := b.State.SetTopicNote(topicID, noteURI); err != nil {
			return err
		}
		if n.URL != "" {
			if err := b.State.SetIssueTopic(series, n.URL, topicID); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("note %q not found in the %s outbox (it may be older than the outbox window)", noteURI, series)
}

// Unbind removes a topic's mapping.
func (b *Bridge) Unbind(topicID int64) error {
	return b.State.DeleteTopicNote(topicID)
}

// Mappings returns the current topic -> note URI bindings.
func (b *Bridge) Mappings() map[int64]string {
	return b.State.AllTopicNotes()
}

func (b *Bridge) seriesConfigured(series string) bool {
	for _, s := range b.Series {
		if s == series {
			return true
		}
	}
	return false
}

// refreshMapping pulls each series' outbox and links topics to issues:
// explicit mappings first, then (only if opted in) title matching.
func (b *Bridge) refreshMapping(ctx context.Context, topics []discourse.Topic) (map[string][]graft.NoteInfo, error) {
	notesBySeries := make(map[string][]graft.NoteInfo, len(b.Series))
	index := make(map[string]graft.NoteInfo) // noteURI -> note
	for _, series := range b.Series {
		oc, err := b.Graft.Outbox(ctx, series)
		if err != nil {
			b.logf(slog.LevelError, "graft outbox failed; skipping series", "series", series, "err", err)
			continue
		}
		notes := graft.ParseNotes(b.GraftHost, oc)
		notesBySeries[series] = notes
		for _, n := range notes {
			index[n.URI] = n
		}
	}

	if err := b.applyExplicit(index); err != nil {
		return nil, err
	}
	if b.Opts.AllowTitleMatching {
		if err := b.applyTitleMatching(topics, notesBySeries); err != nil {
			return nil, err
		}
	}
	return notesBySeries, nil
}

// applyExplicit records admin-provided topic<->note links.
func (b *Bridge) applyExplicit(index map[string]graft.NoteInfo) error {
	for topicID, noteURI := range b.Opts.Explicit {
		series, _, ok := ap.ParseNoteURI(b.GraftHost, noteURI)
		if !ok {
			return fmt.Errorf("explicit mapping for topic %d: %q is not a %s actor note URI", topicID, noteURI, b.GraftHost)
		}
		if _, ok := b.State.TopicNote(topicID); !ok {
			if err := b.State.SetTopicNote(topicID, noteURI); err != nil {
				return err
			}
		}
		if n, ok := index[noteURI]; ok && n.URL != "" {
			if err := b.State.SetIssueTopic(series, n.URL, topicID); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyTitleMatching is the opt-in heuristic fallback.
func (b *Bridge) applyTitleMatching(topics []discourse.Topic, notesBySeries map[string][]graft.NoteInfo) error {
	for series, notes := range notesBySeries {
		for _, t := range topics {
			if _, ok := b.State.TopicNote(t.ID); ok {
				continue
			}
			n, ok := graft.MatchIssueByTitle(notes, t.Title)
			if !ok {
				continue
			}
			if err := b.State.SetTopicNote(t.ID, n.URI); err != nil {
				return err
			}
			if n.URL != "" {
				if err := b.State.SetIssueTopic(series, n.URL, t.ID); err != nil {
					return err
				}
			}
			b.logf(slog.LevelWarn, "mapped topic by title heuristic (insecure)", "topic", t.ID, "series", series)
		}
	}
	return nil
}

// forwardDiscourseToGraft turns new Discourse replies into signed
// Create{Note} activities whose inReplyTo is the topic's Graft issue note.
func (b *Bridge) forwardDiscourseToGraft(ctx context.Context, topics []discourse.Topic) error {
	category := make(map[int64]int64, len(topics))
	slug := make(map[int64]string, len(topics))
	for _, t := range topics {
		category[t.ID] = t.CategoryID
		slug[t.ID] = t.Slug
	}

	posts, err := b.Discourse.LatestPosts(ctx)
	if err != nil {
		return fmt.Errorf("discourse latest posts: %w", err)
	}
	if b.MaxPosts > 0 && len(posts) > b.MaxPosts {
		posts = posts[:b.MaxPosts]
	}

	delivered := 0
	// Latest-first from the API; deliver oldest-first to preserve order.
	for i := len(posts) - 1; i >= 0; i-- {
		if b.Opts.MaxDeliveriesPerPass > 0 && delivered >= b.Opts.MaxDeliveriesPerPass {
			b.logf(slog.LevelWarn, "delivery rate limit reached for this pass", "limit", b.Opts.MaxDeliveriesPerPass)
			break
		}
		p := posts[i]
		// post_number 1 is the topic itself — the issue body, which Graft
		// mirrors create-only and has no reply path for.
		if p.PostNumber <= 1 {
			continue
		}
		// A post authored by the bridge's own bot account is always its
		// own reverse-mirror (see reverseGraftToDiscourse) — never a real
		// reply to forward back.
		if b.Opts.BotUsername != "" && p.Username == b.Opts.BotUsername {
			continue
		}
		if !b.categoryAllowed(category[p.TopicID]) {
			continue
		}
		noteURI, ok := b.State.TopicNote(p.TopicID)
		if !ok {
			continue
		}
		series, _, ok := ap.ParseNoteURI(b.GraftHost, noteURI)
		if !ok {
			continue
		}
		if b.State.PostDelivered(series, p.ID) {
			continue
		}
		content := truncateRunes("**via Discourse, @"+p.Username+":**\n\n"+p.Raw, b.maxContent())
		// Generate trackback URL to the original Discourse post
		// TODO: get BaseURL from config instead of hardcoding
		sourceURL := fmt.Sprintf("https://discourse.cyberwild.org/t/%s/%d/%d", slug[p.TopicID], p.TopicID, p.PostNumber)
		if err := b.Graft.ReplyToIssue(ctx, series, noteURI, content, sourceURL); err != nil {
			b.logf(slog.LevelError, "deliver reply failed", "post", p.ID, "topic", p.TopicID, "err", err)
			continue
		}
		if err := b.State.MarkPostDelivered(series, p.ID); err != nil {
			return err
		}
		if err := b.State.MarkDelivered(hashContent(truncateRunes(content, 200))); err != nil {
			return err
		}
		delivered++
		b.logf(slog.LevelInfo, "delivered discourse post to repo", "post", p.ID, "topic", p.TopicID, "note", noteURI)
	}
	return nil
}

// reverseGraftToDiscourse mirrors repo-origin comments back into the
// matching Discourse topic. Comment notes whose content came from the
// fediverse (i.e. our own outbound posts, echoed back) are dropped.
func (b *Bridge) reverseGraftToDiscourse(ctx context.Context, notes map[string][]graft.NoteInfo) error {
	for series, list := range notes {
		for _, n := range list {
			if n.Kind != "comment" {
				continue
			}
			if b.State.SeenNote(series, n.URI) {
				continue
			}
			if b.State.IsDelivered(hashContent(truncateRunes(n.Summary, 200))) ||
				strings.Contains(n.Summary, "via Fediverse") || strings.Contains(n.Summary, "via Bluesky") {
				if err := b.State.MarkSeenNote(series, n.URI); err != nil {
					return err
				}
				continue
			}
			topicID, ok := b.State.IssueTopic(series, n.URL)
			if !ok {
				// No topic mapping for this issue/patch yet; leave unseen
				// and retry on a later pass.
				continue
			}
			body := truncateRunes("**Comment on the repository:**\n\n"+n.Summary, b.maxContent())
			if _, err := b.Discourse.CreatePost(ctx, topicID, body); err != nil {
				b.logf(slog.LevelError, "create discourse post failed", "topic", topicID, "note", n.URI, "err", err)
				continue
			}
			if err := b.State.MarkSeenNote(series, n.URI); err != nil {
				return err
			}
			b.logf(slog.LevelInfo, "mirrored repo comment to discourse", "topic", topicID, "note", n.URI)
		}
	}
	return nil
}

// categoryAllowed implements the category allowlist. With no allowlist
// configured every category is allowed; with one, an unknown topic is
// denied.
func (b *Bridge) categoryAllowed(categoryID int64) bool {
	if len(b.Opts.AllowedCategories) == 0 {
		return true
	}
	return b.Opts.AllowedCategories[categoryID]
}

func (b *Bridge) maxContent() int {
	if b.Opts.MaxContentRunes <= 0 {
		return 8000
	}
	return b.Opts.MaxContentRunes
}

func (b *Bridge) logf(level slog.Level, msg string, args ...any) {
	if b.Log != nil {
		b.Log.Log(context.Background(), level, msg, args...)
	}
}

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
