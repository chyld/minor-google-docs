package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// A note as one .tar.gz with every file that belongs to it (SPEC §24), in the
// export's layout for one note, so `notes import` brings it back. PrepareNoteArchive
// takes a snapshot (text, tags, files and their sizes); WriteTo streams it, reading
// each file once and refusing any that changed since.

// ErrNoteTrashed: trashed notes can't be downloaded as an archive.
var ErrNoteTrashed = errors.New("restore the note to download it")

const (
	archiveFolderBytes = 200 // folder name, before "notes/"
	archiveFileBytes   = 230 // file name, leaving room for " (2)" within 255
)

// NoteArchive is a snapshot of a note ready to be written as .tar.gz.
type NoteArchive struct {
	Title    string // DeriveTitle: for the folder and the download name
	Missing  int    // files the archive couldn't include (named in MISSING.txt)
	folder   string
	modified time.Time
	body     []byte // note.txt as on disk
	meta     []byte
	manifest []byte
	missing  []byte // MISSING.txt, nil when nothing is missing
	files    []archiveFile
}

type archiveFile struct {
	name string // the entry's path in the archive
	f    *NoteFile
}

// PrepareNoteArchive snapshots a note for the archive.
func (s *Store) PrepareNoteArchive(ctx context.Context, uid, id string) (*NoteArchive, error) {
	m, err := s.noteMeta(ctx, s.DB.R, uid, id)
	if err != nil {
		return nil, err
	}
	if m.TrashedAt != nil {
		return nil, ErrNoteTrashed
	}
	if s.uploadPending(ctx, uid, id) {
		return nil, ErrUploadPending
	}
	raw, err := readCapped(s.notePath(uid, id), MaxBodyBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound // purged meanwhile, or its text is lost (doctor reports it)
	} else if err != nil {
		return nil, err
	}
	body := cleanPageText(string(raw)) // to find the markers; the archive keeps the raw bytes
	tags, err := s.noteTags(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	nf, err := s.resolveNoteFiles(ctx, uid, id, body)
	if err != nil {
		return nil, err
	}
	a := &NoteArchive{Title: DeriveTitle(body), body: raw, modified: time.UnixMilli(m.UpdatedAt)}
	a.folder = "notes/" + capBytes(safeName(a.Title), archiveFolderBytes)

	mn := manifestNote{ID: id, File: a.folder + "/note.txt", Folder: a.folder, Title: a.Title, Pinned: m.Pinned,
		CreatedAt: time.UnixMilli(m.CreatedAt).UTC().Format(time.RFC3339Nano), UpdatedAt: time.UnixMilli(m.UpdatedAt).UTC().Format(time.RFC3339Nano),
		Tags: []string{}, TagColors: map[string]string{}, Attachments: []manifestAttach{}}
	meta := metaFile{Version: 1, ID: id, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Pinned: m.Pinned,
		Tags: []metaTag{}, Attachments: []metaAtt{}}
	for _, t := range tags {
		mn.Tags = append(mn.Tags, t.Name)
		mn.TagColors[t.Name] = t.Color
		meta.Tags = append(meta.Tags, t)
	}
	sums, err := s.attachmentSums(ctx, uid, nf.files)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, f := range nf.files {
		switch {
		case f.superseded:
			continue // another note's copy is in the archive instead
		case f.missing:
			mn.Missing = append(mn.Missing, manifestMissing{Ref: f.Att.Ref, Filename: f.Att.Filename, Reason: "unreadable", shown: f.shown})
			continue
		}
		name := a.folder + "/files/" + uniqueFileName(used, capFileName(exportFileName(f.Att.Filename)))
		a.files = append(a.files, archiveFile{name: name, f: f})
		mn.Attachments = append(mn.Attachments, manifestAttach{Ref: f.Att.Ref, Filename: f.Att.Filename, File: name, Mime: f.Att.Mime, Size: f.size})
		meta.Attachments = append(meta.Attachments, metaAtt{Ref: f.Att.Ref, Ext: f.Att.Ext, Filename: f.Att.Filename,
			Mime: f.Att.Mime, SizeBytes: f.size, SHA256: sums[f.Att.ID], CreatedAt: f.Att.CreatedAt})
	}
	// images the text shows that exist nowhere: named by their caption
	captions := markerCaptions(body)
	for _, ref := range markerRefs(body) {
		if nf.byRef[ref] == nil {
			mn.Missing = append(mn.Missing, manifestMissing{Ref: ref, Filename: captions[ref], Reason: "not found", shown: true})
		}
	}
	a.Missing = len(mn.Missing)
	if a.Missing > 0 {
		var b strings.Builder
		b.WriteString("These files of the note are missing on the server, so they aren't in this archive:\n\n")
		for _, x := range mn.Missing {
			name := x.Filename
			if name == "" {
				name = "(image without a name)"
			}
			if x.shown { // the marker, as it is in the note's text
				fmt.Fprintf(&b, "  %s  [image:%s]\n", name, x.Ref)
			} else {
				fmt.Fprintf(&b, "  %s\n", name)
			}
		}
		b.WriteString("\nIn the app, Account & data → Check my notes looks for them.\n")
		a.missing = []byte(b.String())
	}
	if a.meta, err = json.MarshalIndent(meta, "", "  "); err != nil {
		return nil, err
	}
	a.meta = append(a.meta, '\n')
	if a.manifest, err = json.MarshalIndent(map[string]any{"version": 2, "exported_at": time.Now().UTC().Format(time.RFC3339),
		"notes": []manifestNote{mn}}, "", "  "); err != nil {
		return nil, err
	}
	a.manifest = append(a.manifest, '\n')
	return a, nil
}

// attachmentSums: the files' content hashes in one query (a row deleted meanwhile has
// none; its file is still in the archive, as snapshotted).
func (s *Store) attachmentSums(ctx context.Context, uid string, files []*NoteFile) (map[string]string, error) {
	out := map[string]string{}
	if len(files) == 0 {
		return out, nil
	}
	ids := make([]any, 0, len(files)+1)
	ids = append(ids, uid)
	for _, f := range files {
		ids = append(ids, f.Att.ID)
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id, COALESCE(sha256,'') FROM attachments WHERE user_id=? AND id IN (?`+
		strings.Repeat(",?", len(files)-1)+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, err
		}
		out[id] = sum
	}
	return out, rows.Err()
}

// markerCaptions: each ref's caption on its first marker line.
func markerCaptions(body string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(body, "\n") {
		if m := MarkerRe.FindStringSubmatch(line); m != nil {
			if _, ok := out[m[1]]; !ok {
				out[m[1]] = m[2]
			}
		}
	}
	return out
}

// capBytes cuts s to at most n bytes on a rune boundary (file systems limit a name
// to 255 bytes, and an emoji is 4).
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimRight(s, ". ")
}

// capFileName caps a file name's bytes, keeping its extension when it is short.
func capFileName(name string) string {
	if len(name) <= archiveFileBytes {
		return name
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 16 {
		ext = name[i:]
	}
	base := capBytes(strings.TrimSuffix(name, ext), archiveFileBytes-len(ext))
	if base == "" {
		base = "file"
	}
	return base + ext
}

// Filename is the download name.
func (a *NoteArchive) Filename() string {
	return capBytes(safeName(a.Title), archiveFolderBytes) + ".tar.gz"
}

// WriteTo writes the archive. Any error after the first byte means the output is
// incomplete: the caller must abort the response, never end it normally.
func (a *NoteArchive) WriteTo(w io.Writer) (int64, error) {
	if hook := archiveBeforeWrite.Load(); hook != nil {
		(*hook)(a)
	}
	cw := &countWriter{w: w}
	gz, err := gzip.NewWriterLevel(cw, gzip.BestSpeed) // attachments are mostly compressed already
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(gz)
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: a.modified, Format: tar.FormatPAX})
	}
	small := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: a.modified, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	err = func() error {
		for _, d := range []string{"notes", a.folder} {
			if err := dir(d); err != nil {
				return err
			}
		}
		if err := small(a.folder+"/note.txt", a.body); err != nil {
			return err
		}
		if err := small(a.folder+"/meta.json", a.meta); err != nil {
			return err
		}
		if len(a.files) > 0 {
			if err := dir(a.folder + "/files"); err != nil {
				return err
			}
		}
		for _, af := range a.files {
			if err := a.writeFile(tw, af); err != nil {
				return err
			}
		}
		if a.missing != nil {
			if err := small("MISSING.txt", a.missing); err != nil {
				return err
			}
		}
		if err := small("manifest.json", a.manifest); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}()
	return cw.n, err
}

// writeFile copies one file, exactly as snapshotted: a file that changed since (another
// inode, or a different size) is refused, never written short or long.
func (a *NoteArchive) writeFile(tw *tar.Writer, af archiveFile) error {
	fh, err := af.f.openSnapshotted(true)
	if err != nil {
		return err
	}
	defer fh.Close()
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: af.name, Mode: 0o644, Size: af.f.size, Format: tar.FormatPAX,
		ModTime: time.UnixMilli(af.f.Att.CreatedAt)}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, fh, af.f.size); err != nil {
		return fmt.Errorf("%s: %w", af.f.Att.Filename, err)
	}
	return nil
}

// archiveBeforeWrite lets tests change files between the snapshot and the write
// (atomic: a reset request's handler may still be writing when a test clears it).
var archiveBeforeWrite atomic.Pointer[func(*NoteArchive)]
