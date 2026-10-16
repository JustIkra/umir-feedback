// Source: umir-factory reference/durable@26166c8ba197

package postgres

import "errors"

// ErrLeaseLost is returned when the fence of a lease or outbox claim no longer
// belongs to the caller: the term expired and someone else took over, or the
// lease was never held.
var ErrLeaseLost = errors.New("lease lost")

// ignoreErr documents a deliberately ignored error from a cleanup path, where
// the original failure (or the success) is what the caller reports.
func ignoreErr(error) {}
