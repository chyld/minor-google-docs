CREATE TABLE users(
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE sessions(
  token_hash BLOB PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  absolute_expires_at INTEGER NOT NULL,
  user_agent TEXT
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_exp ON sessions(expires_at);

CREATE TABLE notes(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  preview TEXT NOT NULL DEFAULT '',
  pinned INTEGER NOT NULL DEFAULT 0,
  trashed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  size_bytes INTEGER NOT NULL,
  content_sha256 TEXT NOT NULL,
  UNIQUE(id, user_id)
);
CREATE INDEX notes_list ON notes(user_id, trashed_at, pinned, updated_at);

CREATE TABLE tags(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL COLLATE NOCASE,
  color TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(user_id, name),
  UNIQUE(id, user_id)
);

CREATE TABLE note_tags(
  note_id TEXT NOT NULL,
  tag_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  PRIMARY KEY(note_id, tag_id),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE,
  FOREIGN KEY(tag_id, user_id) REFERENCES tags(id, user_id) ON DELETE CASCADE
);
CREATE INDEX note_tags_tag ON note_tags(tag_id);

CREATE TABLE attachments(
  id TEXT PRIMARY KEY,
  ref TEXT NOT NULL,
  user_id TEXT NOT NULL,
  note_id TEXT NOT NULL,
  filename TEXT NOT NULL,
  mime TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes INTEGER NOT NULL,          -- -1 while length is deferred
  sha256 TEXT,
  state TEXT NOT NULL CHECK (state IN ('uploading','ready','deleting')),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(user_id, ref),
  FOREIGN KEY(note_id, user_id) REFERENCES notes(id, user_id) ON DELETE CASCADE
);
CREATE INDEX attachments_note ON attachments(note_id);
CREATE INDEX attachments_state ON attachments(state);

CREATE TABLE audit_log(
  id INTEGER PRIMARY KEY,
  at INTEGER NOT NULL,
  actor TEXT NOT NULL,
  event TEXT NOT NULL,
  detail TEXT
);
