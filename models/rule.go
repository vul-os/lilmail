package models

// MailRule is a per-account inbound filter (Gmail-parity "Filters"). It is the
// /v1 wire shape brokered to vulos-mail's authoritative rule store and rendered
// by the mail-ui Filters surface. Field order/tags must stay in lockstep with
// vulos-mail's internal/mailrules types and the mail-ui client.
//
// Safety: the action set is deliberately curated. There is NO auto-forward and NO
// permanent-delete action — both are abuse / data-loss vectors. "trash" moves a
// message to the recoverable Trash folder, never expunges it. lilmail rejects
// forbidden action types before they ever reach the network (defence in depth;
// vulos-mail rejects them again at persist time).
type MailRule struct {
	ID         string          `json:"id,omitempty"` // server-assigned; omit on create
	Name       string          `json:"name"`
	Enabled    bool            `json:"enabled"`
	Match      string          `json:"match"` // "all" | "any"
	Conditions []RuleCondition `json:"conditions"`
	Actions    []RuleAction    `json:"actions"`
}

// RuleCondition is one predicate over a message field.
//
//	field: from | to | subject | body | attachment
//	op:    contains | not_contains | equals | matches | exists
//	       ("exists" is only meaningful for field "attachment"; value ignored)
type RuleCondition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value,omitempty"`
}

// RuleAction is one thing to do when a rule matches.
//
//	type:  label | move | mark_read | star | archive | trash | stop
//	       (value = target folder/label for label & move; ignored otherwise)
type RuleAction struct {
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}

// ForbiddenRuleActions are action types lilmail refuses to broker: auto-forward
// (exfiltration) and permanent delete (data loss). Kept here so the handler and
// any future caller share one denylist.
var ForbiddenRuleActions = map[string]bool{
	"forward": true, "auto_forward": true, "redirect": true,
	"delete": true, "delete_forever": true, "expunge": true, "purge": true,
}
