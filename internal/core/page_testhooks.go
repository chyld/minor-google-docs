package core

import "sync/atomic"

// SetPageBeforeWriteForTest lets other packages' tests change files between a page's
// snapshot and its write (to prove a broken download is aborted, not truncated).
func SetPageBeforeWriteForTest(f func(*Page)) { setHook(&pageBeforeWrite, f) }

// PageFilePaths lists the on-disk paths of the files a page will embed (tests).
func (p *Page) PageFilePaths() []string {
	var out []string
	for _, f := range p.files {
		if !f.missing && !f.superseded {
			out = append(out, f.path)
		}
	}
	return out
}

// SetArchiveBeforeWriteForTest lets other packages' tests change files between an
// archive's snapshot and its write.
func SetArchiveBeforeWriteForTest(f func(*NoteArchive)) { setHook(&archiveBeforeWrite, f) }

func setHook[T any](p *atomic.Pointer[func(T)], f func(T)) {
	if f == nil {
		p.Store(nil)
	} else {
		p.Store(&f)
	}
}

// ArchiveFilePaths lists the on-disk paths of the files an archive will carry (tests).
func (a *NoteArchive) ArchiveFilePaths() []string {
	var out []string
	for _, af := range a.files {
		out = append(out, af.f.path)
	}
	return out
}
