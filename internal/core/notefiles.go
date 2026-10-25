package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// A note's files as the editor shows them, shared by the web page (SPEC §23) and the
// archive (SPEC §24): this note's ready attachments, plus, for an image the text shows
// that this note has no readable copy of, another of the user's notes' copy.

// NoteFile is one file of a note, opened once to prove it can be read.
type NoteFile struct {
	Att        Attachment
	path       string
	size       int64
	own        bool        // in this note (otherwise a copy from another note, shown in the text)
	shown      bool        // shown in the text by a marker
	info       os.FileInfo // the file as snapshotted: the write refuses a different one
	missing    bool        // no readable file on disk
	superseded bool        // this note's unreadable copy, replaced by another note's (not listed)
	n          int
}

type noteFiles struct {
	files []*NoteFile
	byRef map[string]*NoteFile // refs the text shows → the file shown (absent: found nowhere)
}

// resolveNoteFiles lists a note's files for body (the note's text, LF line ends).
func (s *Store) resolveNoteFiles(ctx context.Context, uid, id, body string) (*noteFiles, error) {
	nf := &noteFiles{byRef: map[string]*NoteFile{}}
	atts, err := s.ListAttachments(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	own := map[string]*NoteFile{}
	for _, a := range atts {
		own[a.Ref] = nf.add(s, uid, a, true)
	}
	// images the text shows from another note (a copy not made yet, or this note's
	// copy can't be read): the same lookup as the editor's
	for _, ref := range markerRefs(body) {
		if f := own[ref]; f != nil && !f.missing {
			f.shown = true
			nf.byRef[ref] = f
			continue
		}
		c, err := s.pageCopy(ctx, uid, ref, id)
		if err != nil {
			return nil, err
		}
		if c != nil {
			if f := own[ref]; f != nil {
				f.superseded = true // the copy stands in for this note's unreadable file
			}
			f := nf.add(s, uid, *c, false)
			f.shown = true
			nf.byRef[ref] = f
		} else if f := own[ref]; f != nil {
			f.shown = true
			nf.byRef[ref] = f
		}
	}
	return nf, nil
}

// add adds a file to the inventory. A file that can't be opened counts as missing,
// so the snapshot never promises bytes the write can't read.
func (nf *noteFiles) add(s *Store, uid string, a Attachment, own bool) *NoteFile {
	f := &NoteFile{Att: a, own: own, n: len(nf.files), missing: true}
	if a.Ext != "" {
		f.path = s.attPath(uid, &a)
		if st, ok := openable(f.path); ok {
			f.info, f.size, f.missing = st, st.Size(), false
		}
	}
	nf.files = append(nf.files, f)
	return f
}

// openSnapshotted reopens a file for writing it out: it must still be the file the
// snapshot saw (same inode) and at least its size, so a file moved onto the path
// meanwhile is refused rather than read. exact also refuses one that grew.
func (f *NoteFile) openSnapshotted(exact bool) (*os.File, error) {
	fh, st, err := openRegularFile(f.path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(st, f.info) || st.Size() < f.size || (exact && st.Size() != f.size) {
		fh.Close()
		return nil, fmt.Errorf("%s changed after the download was prepared", f.Att.Filename)
	}
	return fh, nil
}

// ErrUploadPending: an upload to the note is still running.
var ErrUploadPending = errors.New("an upload to this note is still running: wait for it to finish")

// uploads quiet for this long count as abandoned for the page and the archive
const uploadQuiet = 5 * time.Minute

// uploadPending: a busy upload would be missing from a download (rows left behind by
// an abandoned upload stop counting after a few minutes; the janitor removes them).
func (s *Store) uploadPending(ctx context.Context, uid, id string) bool {
	return s.exists(ctx, `SELECT 1 FROM attachments WHERE note_id=? AND user_id=? AND state='uploading' AND updated_at>?`,
		id, uid, now()-int64(uploadQuiet/time.Millisecond))
}

// noteTags: a note's tags in the app's order (by name).
func (s *Store) noteTags(ctx context.Context, uid, id string) ([]metaTag, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT t.name,t.color FROM tags t JOIN note_tags nt ON nt.tag_id=t.id
 WHERE nt.note_id=? AND t.user_id=? ORDER BY t.name`, id, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []metaTag
	for rows.Next() {
		var t metaTag
		if err := rows.Scan(&t.Name, &t.Color); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// readCapped reads a note's text (never through a symlink or from a FIFO), refusing
// anything larger than limit.
func readCapped(path string, limit int64) ([]byte, error) {
	f, _, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, Invalid("this note is larger than %d MiB", limit>>20)
	}
	return b, nil
}

// openRegularFile opens a file for reading without following a symlink or waiting
// on a FIFO, and only if it is a regular file.
func openRegularFile(path string) (*os.File, os.FileInfo, error) {
	fh, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := fh.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if err != nil {
		fh.Close()
		return nil, nil, err
	}
	return fh, st, nil
}

// openable proves a file can be read and returns what it is.
func openable(path string) (os.FileInfo, bool) {
	fh, st, err := openRegularFile(path)
	if err != nil {
		return nil, false
	}
	fh.Close()
	return st, true
}

// pageCopy finds a ready copy of ref in another of the user's notes that can be read.
func (s *Store) pageCopy(ctx context.Context, uid, ref, noteID string) (*Attachment, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT `+attCols+` FROM attachments
 WHERE user_id=? AND ref=? AND state='ready' AND note_id!=? AND ext!='' ORDER BY created_at DESC, id`, uid, ref, noteID)
	if err != nil {
		return nil, err
	}
	var cands []Attachment
	for rows.Next() {
		a, err := scanAtt(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range cands {
		if _, ok := openable(s.attPath(uid, &cands[i])); ok {
			return &cands[i], nil
		}
	}
	return nil, nil
}
