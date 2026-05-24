// handlers/api/threading.go — JWZ (Jamie Zawinski) message threading algorithm.
//
// Reference: https://www.jwz.org/doc/threading.html
//
// The algorithm operates in five stages:
//  1. Build an id_table: map[messageID]*Container
//  2. Link messages by References and In-Reply-To
//  3. Collect the root set (containers whose parent == nil)
//  4. Prune empty containers (no message, no children)
//  5. Group the root set by normalized subject
//
// The result is a []models.Thread sorted by the latest-message date descending.
package api

import (
	"lilmail/models"
	"regexp"
	"sort"
	"strings"
)

// container is an internal node used during the JWZ algorithm.
type container struct {
	msg      *models.Email
	parent   *container
	children []*container
}

// addChild appends c as a child of p and sets c.parent.
func (p *container) addChild(c *container) {
	c.parent = p
	p.children = append(p.children, c)
}

// removeChild removes c from p.children (does not clear c.parent).
func (p *container) removeChild(c *container) {
	out := p.children[:0]
	for _, ch := range p.children {
		if ch != c {
			out = append(out, ch)
		}
	}
	p.children = out
}

// hasAncestor reports whether p is an ancestor of c (loop guard).
func hasAncestor(c, p *container) bool {
	for cur := c.parent; cur != nil; cur = cur.parent {
		if cur == p {
			return true
		}
	}
	return false
}

// reSubjectStrip matches leading noise like "Re:", "Fwd:", "Fw:", etc. and
// surrounding brackets/parens (case-insensitive, greedy).
var reSubjectStrip = regexp.MustCompile(`(?i)^(re|fwd?|sv|aw|antw|wg|回复|转发)\s*(\[.*?\]|\(.*?\))?\s*:\s*`)

// normalizeSubject strips reply/forward prefixes for subject-grouping.
func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		stripped := reSubjectStrip.ReplaceAllString(s, "")
		stripped = strings.TrimSpace(stripped)
		if stripped == s {
			break
		}
		s = stripped
	}
	return strings.ToLower(s)
}

// ThreadMessages applies the JWZ algorithm to emails and returns a
// []models.Thread sorted by latest-message date descending. Single-message
// conversations still appear as Thread{Count:1}.
func ThreadMessages(emails []models.Email) []models.Thread {
	if len(emails) == 0 {
		return nil
	}

	// ------------------------------------------------------------------ step 1
	// Build id_table.  For duplicate Message-IDs keep the first real message.
	idTable := make(map[string]*container, len(emails))

	getOrCreate := func(id string) *container {
		if id == "" {
			return nil
		}
		if c, ok := idTable[id]; ok {
			return c
		}
		c := &container{}
		idTable[id] = c
		return c
	}

	for i := range emails {
		e := &emails[i]
		msgID := e.MessageID

		var c *container
		if msgID != "" {
			if existing, ok := idTable[msgID]; ok {
				if existing.msg == nil {
					existing.msg = e // fill empty placeholder
				}
				c = existing
			} else {
				c = &container{msg: e}
				idTable[msgID] = c
			}
		} else {
			// No Message-ID: create an anonymous container not in the id_table.
			c = &container{msg: e}
		}

		// ---------------------------------------------------------------- step 2
		// Link references chain as parent→child.
		refs := e.References
		// Append In-Reply-To as a final reference if not already present.
		if e.InReplyTo != "" {
			found := false
			for _, r := range refs {
				if r == e.InReplyTo {
					found = true
					break
				}
			}
			if !found {
				refs = append(refs, e.InReplyTo)
			}
		}

		var prev *container
		for _, ref := range refs {
			rc := getOrCreate(ref)
			if rc == nil {
				continue
			}
			// Link prev→rc if rc has no parent yet and it wouldn't create a loop.
			if prev != nil && rc.parent == nil && rc != prev && !hasAncestor(prev, rc) {
				prev.addChild(rc)
			}
			prev = rc
		}

		// Set the message's container parent to the last reference container.
		if prev != nil && c != prev {
			// Only re-parent if c has no parent yet and it wouldn't loop.
			if c.parent == nil && !hasAncestor(prev, c) {
				prev.addChild(c)
			}
		}
	}

	// ------------------------------------------------------------------ step 3
	// Collect the root set: containers with no parent.
	var roots []*container
	for _, c := range idTable {
		if c.parent == nil {
			roots = append(roots, c)
		}
	}
	// Also include anonymous containers (no Message-ID) that aren't in idTable.
	// They were skipped above; add them directly as roots.
	for i := range emails {
		e := &emails[i]
		if e.MessageID == "" {
			roots = append(roots, &container{msg: e})
		}
	}

	// ------------------------------------------------------------------ step 4
	// Prune empty containers recursively.
	roots = pruneEmpties(roots)

	// ------------------------------------------------------------------ step 5
	// Group root set by normalized subject.
	roots = groupBySubject(roots)

	// Convert container trees to Thread slices.
	threads := make([]models.Thread, 0, len(roots))
	for _, root := range roots {
		var msgs []models.Email
		collectMessages(root, &msgs)
		if len(msgs) == 0 {
			continue
		}
		// Sort messages within thread by date ascending.
		sort.Slice(msgs, func(i, j int) bool {
			return msgs[i].Date.Before(msgs[j].Date)
		})
		latest := msgs[len(msgs)-1]
		threads = append(threads, models.Thread{
			Root:     msgs[0],
			Messages: msgs,
			Count:    len(msgs),
			Latest:   latest,
		})
	}

	// Sort threads: latest message date descending.
	sort.Slice(threads, func(i, j int) bool {
		return threads[i].Latest.Date.After(threads[j].Latest.Date)
	})

	return threads
}

// pruneEmpties removes containers that carry no message and have no children,
// and hoists children of empty containers up to their grandparent.
func pruneEmpties(roots []*container) []*container {
	var out []*container
	for _, c := range roots {
		c.children = pruneEmpties(c.children)
		if c.msg == nil {
			if len(c.children) == 0 {
				// Discard entirely.
				continue
			}
			if len(c.children) == 1 || c.parent != nil {
				// Promote single child (or all children) up.
				out = append(out, c.children...)
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// collectMessages does a depth-first traversal of the container tree and
// appends every message it finds to msgs.
func collectMessages(c *container, msgs *[]models.Email) {
	if c.msg != nil {
		*msgs = append(*msgs, *c.msg)
	}
	for _, ch := range c.children {
		collectMessages(ch, msgs)
	}
}

// groupBySubject merges root containers that share a normalized subject into a
// single synthetic root. This implements the optional step 5 from JWZ.
func groupBySubject(roots []*container) []*container {
	type entry struct {
		c       *container
		isReply bool // subject started with Re:/Fwd: etc.
	}
	tbl := make(map[string]*entry, len(roots))

	for _, c := range roots {
		subj := ""
		if c.msg != nil {
			subj = normalizeSubject(c.msg.Subject)
		} else if len(c.children) > 0 {
			// Find first non-nil message in children.
			var findSubj func(*container) string
			findSubj = func(x *container) string {
				if x.msg != nil {
					return normalizeSubject(x.msg.Subject)
				}
				for _, ch := range x.children {
					if s := findSubj(ch); s != "" {
						return s
					}
				}
				return ""
			}
			subj = findSubj(c)
		}
		if subj == "" {
			continue
		}
		isReply := false
		if c.msg != nil {
			isReply = reSubjectStrip.MatchString(c.msg.Subject)
		}
		if existing, ok := tbl[subj]; ok {
			// Merge c into existing.
			if existing.isReply && !isReply {
				// existing is a reply, c is the root — make c the new root.
				existing.c.parent = c
				c.children = append(c.children, existing.c)
				tbl[subj] = &entry{c: c, isReply: false}
			} else {
				existing.c.addChild(c)
			}
		} else {
			tbl[subj] = &entry{c: c, isReply: isReply}
		}
	}

	// Collect merged roots (containers still without a parent).
	var out []*container
	for _, e := range tbl {
		if e.c.parent == nil {
			out = append(out, e.c)
		}
	}
	// Any root whose subject was empty goes in as-is.
	for _, c := range roots {
		subj := ""
		if c.msg != nil {
			subj = normalizeSubject(c.msg.Subject)
		}
		if subj == "" && c.parent == nil {
			out = append(out, c)
		}
	}
	return out
}
