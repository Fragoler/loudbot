package suggestion

import (
	"strconv"
	"strings"
)

// Callback data namespaces. An id travels back from the client inside them, so
// every handler re-checks ownership and status rather than trusting what arrives.
const (
	prefixEdit     = "sug:edit:"
	prefixWithdraw = "sug:drop:"
	prefixApprove  = "sug:appr:"
	prefixConfirm  = "sug:ok:"
	prefixAbort    = "sug:back:"
	prefixDecline  = "sug:decl:"
)

// Action is what a tapped button asks for.
type Action string

const (
	// ActionEdit and ActionWithdraw belong to the author.
	ActionEdit     Action = "edit"
	ActionWithdraw Action = "withdraw"
	// ActionApprove asks for confirmation; ActionConfirm publishes, ActionAbort
	// puts the original buttons back.
	ActionApprove Action = "approve"
	ActionConfirm Action = "confirm"
	ActionAbort   Action = "abort"
	ActionDecline Action = "decline"
)

var prefixes = map[Action]string{
	ActionEdit:     prefixEdit,
	ActionWithdraw: prefixWithdraw,
	ActionApprove:  prefixApprove,
	ActionConfirm:  prefixConfirm,
	ActionAbort:    prefixAbort,
	ActionDecline:  prefixDecline,
}

// Callback builds the data of a button acting on one suggestion.
func Callback(action Action, id int64) string {
	return prefixes[action] + strconv.FormatInt(id, 10)
}

// ParseCallback reads an action and a suggestion id back. ok is false for data
// that belongs to something else entirely.
func ParseCallback(data string) (Action, int64, bool) {
	data = strings.TrimSpace(data)

	for action, prefix := range prefixes {
		raw, found := strings.CutPrefix(data, prefix)
		if !found {
			continue
		}

		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return "", 0, false
		}

		return action, id, true
	}

	return "", 0, false
}

// Owns reports whether the data is a suggestion button at all, so the transport
// can tell it apart from the comment flow's callbacks.
func Owns(data string) bool {
	return strings.HasPrefix(strings.TrimSpace(data), "sug:")
}

// String makes an Action printable in logs.
func (a Action) String() string {
	return string(a)
}
