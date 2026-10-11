// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package ownership

import (
	"sync"
	"time"
)

// DefaultJournalTTL is how long the journal remembers a create. Recovery is
// meant for the reconcile that follows a lost response, so an hour is ample.
const DefaultJournalTTL = time.Hour

// A CreateKey identifies one CREATE USER or CREATE ROLE that an MR asked
// for: the kind, the MR's UID, the entity name and the scope it ran in (the
// ProviderConfig identity and the server UUID), so that a record never
// carries over to another name, another config or another server.
type CreateKey struct {
	Kind  string
	UID   string
	Name  string
	Scope string
}

type createState int

const (
	stateInFlight createState = iota + 1
	stateUnknown
	stateSucceeded
)

type journalEntry struct {
	state createState
	id    string
	at    time.Time
}

// Evidence is what the journal knows about a create when Observe finds the
// entity without an owned-id.
type Evidence struct {
	// Recorded is set when this provider process sent the CREATE for the key
	// and its outcome was not a definite rejection: it succeeded, or it may
	// have run (a transport error, a timeout or 159).
	Recorded bool
	// ID is the UUID read back after a successful CREATE, or "" when the
	// outcome is unknown.
	ID string
}

// A Journal records the CREATE USER and CREATE ROLE statements this provider
// process sent, so that ownership row 5 (Recovered) can tell a create whose
// response was lost from an annotation a tenant wrote.
//
// The create annotations that row 5 reads (external-create-pending,
// create-outcome) are writable by anyone who can edit the MR. A tenant who
// points an MR at an existing user and forges them must not be able to make
// the provider claim, and then alter or drop, that user. Recovery therefore
// also needs a journal record of a CREATE that this process sent for the same
// MR, name and scope; a record with a read-back UUID only recovers exactly
// that UUID. The journal lives in memory only: after a provider restart a
// lost create is not recovered automatically, and the entity is reported as
// NotOwned with a hint to adopt it by UUID.
type Journal struct {
	now func() time.Time
	ttl time.Duration

	mu      sync.Mutex
	entries map[CreateKey]journalEntry
}

// NewJournal returns an empty journal.
func NewJournal() *Journal {
	return &Journal{now: time.Now, ttl: DefaultJournalTTL, entries: map[CreateKey]journalEntry{}}
}

// DefaultJournal is the process-wide journal the controllers share.
var DefaultJournal = NewJournal()

func (j *Journal) put(k CreateKey, s createState, id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.expireLocked()
	j.entries[k] = journalEntry{state: s, id: id, at: j.now()}
}

func (j *Journal) expireLocked() {
	now := j.now()
	for k, e := range j.entries {
		if now.Sub(e.at) > j.ttl {
			delete(j.entries, k)
		}
	}
}

// Begin records that the CREATE for k is about to be sent.
func (j *Journal) Begin(k CreateKey) { j.put(k, stateInFlight, "") }

// Succeeded records that the CREATE for k succeeded and the entity has the
// UUID id.
func (j *Journal) Succeeded(k CreateKey, id string) { j.put(k, stateSucceeded, id) }

// Unknown records that the CREATE for k may or may not have run.
func (j *Journal) Unknown(k CreateKey) { j.put(k, stateUnknown, "") }

// Rejected records that the server definitely rejected the CREATE for k: no
// entity was created, so nothing may be recovered.
func (j *Journal) Rejected(k CreateKey) { j.Forget(k) }

// Forget removes the record for k, for example once the entity is owned.
func (j *Journal) Forget(k CreateKey) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.entries, k)
}

// Evidence returns what the journal knows about the create for k.
func (j *Journal) Evidence(k CreateKey) Evidence {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.expireLocked()
	e, ok := j.entries[k]
	if !ok {
		return Evidence{}
	}
	if e.state == stateSucceeded {
		return Evidence{Recorded: true, ID: e.id}
	}
	return Evidence{Recorded: true}
}
