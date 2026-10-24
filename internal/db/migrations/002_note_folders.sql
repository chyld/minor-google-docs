-- One folder per note (SPEC §3). Applied by the layout conversion, after its backup.
-- An image may now exist once per note (copies keep their ref); one ref still means
-- one image content per user, enforced by ref_content.
CREATE TABLE attachments_v2(
  id TEXT PRIMARY KEY,
  ref TEXT NOT NULL,
  user_id TEXT NOT NULL,
  note_id TEXT NOT NULL,
  filename TEXT NOT NULL,
  mime TEXT NOT NULL DEFAULT 'application/octet-stream',
  ext TEXT NOT NULL DEFAULT '',         -- disk extension from the sniffed type, set before the file is moved
  size_bytes INTEGER NOT NULL,          -- -1 while length is deferred
  sha256 TEXT,
  state TEXT NOT NULL CHECK (state IN ('uploading','ready','deleting')),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(note_id, ref),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE
);
INSERT INTO attachments_v2(id,ref,user_id,note_id,filename,mime,size_bytes,sha256,state,created_at,updated_at)
  SELECT id,ref,user_id,note_id,filename,mime,size_bytes,sha256,state,created_at,updated_at FROM attachments;
DROP TABLE attachments;
ALTER TABLE attachments_v2 RENAME TO attachments;
CREATE INDEX attachments_note ON attachments(note_id);
CREATE INDEX attachments_state ON attachments(state);
CREATE INDEX attachments_ref ON attachments(user_id, ref);

CREATE TABLE ref_content(
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  ref TEXT NOT NULL,
  sha256 TEXT NOT NULL DEFAULT '',      -- '' until the first copy's hash is known
  PRIMARY KEY(user_id, ref)
);
INSERT INTO ref_content(user_id, ref, sha256)
  SELECT user_id, ref, COALESCE(sha256,'') FROM attachments;

-- markers whose image still has to be copied into the note's own folder
CREATE TABLE pending_copies(
  note_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  ref TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(note_id, ref),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE
);
CREATE INDEX pending_copies_ref ON pending_copies(user_id, ref);

-- notes whose meta.json must be rewritten (tag rename/recolour/delete fan-out, failed writes)
CREATE TABLE meta_dirty(
  note_id TEXT PRIMARY KEY REFERENCES notes(id) ON DELETE CASCADE
);

CREATE TABLE settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
