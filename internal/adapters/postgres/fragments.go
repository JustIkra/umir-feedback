// Source: umir-factory reference/durable@26166c8ba197

package postgres

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed storage_meta.sql
var storageMetaSQL string

//go:embed worker_leases.sql
var workerLeasesSQL string

//go:embed outbox.sql
var outboxSQL string

//go:embed inbox.sql
var inboxSQL string

// StorageMetaSQL is the DDL of storage_meta, including the first generation.
func StorageMetaSQL() string { return storageMetaSQL }

// WorkerLeasesSQL is the DDL of worker_leases.
func WorkerLeasesSQL() string { return workerLeasesSQL }

// OutboxSQL is the DDL of outbox and of its claim fence sequence.
func OutboxSQL() string { return outboxSQL }

// InboxSQL is the DDL of inbox.
func InboxSQL() string { return inboxSQL }

const includePrefix = "-- umir:include "

// fragment returns the DDL for an include directive name.
func fragment(name string) (string, bool) {
	switch name {
	case "storage_meta":
		return storageMetaSQL, true
	case "worker_leases":
		return workerLeasesSQL, true
	case "outbox":
		return outboxSQL, true
	case "inbox":
		return inboxSQL, true
	}
	return "", false
}

// includeMarker is the substring that makes a line a candidate directive.
const includeMarker = "umir:include"

// expandIncludes replaces every line "-- umir:include <name>" by the fragment.
// A line that holds the marker but is not a well-formed directive (a typo such
// as "--umir:include outbox", an indented directive, a trailing comment) is an
// error naming the line, so that a misspelt directive cannot silently drop a
// fragment from the schema. An unknown fragment name is an error too.
func expandIncludes(sql string) (string, error) {
	if !strings.Contains(sql, includeMarker) {
		return sql, nil
	}
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if !strings.Contains(line, includeMarker) {
			continue
		}
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r \t"), includePrefix)
		if !ok {
			return "", fmt.Errorf("line %d: %q is not a directive; write it as \"-- umir:include <fragment>\" at the start of the line", i+1, includeMarker)
		}
		name := strings.TrimSpace(rest)
		text, known := fragment(name)
		if !known {
			return "", fmt.Errorf("line %d: unknown fragment %q", i+1, name)
		}
		lines[i] = strings.TrimRight(text, "\n")
	}
	return strings.Join(lines, "\n"), nil
}
