package core

import (
	"context"
	"database/sql"
)

// Owner reads of the knowledge base. The owner is no member and has no principal: these read what the project (or the
// team's own notes: `project` may be a team's segment id) holds, every class, `private` included. The owner can already read
// all of it through backup and audit; these are the admin routes GET /kb/notes, /kb/notes/{id} and /kb/faq. Read-only.
// Pending submissions are not notes and never appear.

func ownerReadable(project int64) []readable { return []readable{{project: project, all: allClasses}} }

// OwnerListNotes pages through a project's (or a team segment's) notes: class and entry kind filters optional.
func (s *Store) OwnerListNotes(ctx context.Context, project int64, class Class, kind, after string, limit int) (pg NotePage, err error) {
	if limit, err = checkListArgs(class, limit); err != nil {
		return
	}
	if kind != "" && !validKind(kind) {
		return pg, ErrInvalid{"kind"}
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "projects", project); e != nil {
			return e
		}
		var e error
		pg, e = listNotes(tx, 0, project, ownerReadable(project), class, kind, after, limit)
		return e
	})
	return
}

// OwnerGetNote reads one note by id, with its body and provenance.
func (s *Store) OwnerGetNote(ctx context.Context, id int64) (n Note, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var e error
		n, e = getNote(tx, id)
		return e
	})
	return
}

// OwnerListFAQ pages through the faq notes of a project or a team segment, newest first.
func (s *Store) OwnerListFAQ(ctx context.Context, project int64, after string) (pg FAQPage, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "projects", project); e != nil {
			return e
		}
		var e error
		pg, e = faqPage(tx, 0, project, ownerReadable(project), after)
		return e
	})
	return
}
