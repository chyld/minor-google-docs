package core

// SetPageBeforeWriteForTest lets other packages' tests change files between a page's
// snapshot and its write (to prove a broken download is aborted, not truncated).
func SetPageBeforeWriteForTest(f func(*Page)) { pageBeforeWrite = f }

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
