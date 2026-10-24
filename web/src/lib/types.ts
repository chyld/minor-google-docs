// API contract — mirrors the Go structs in internal/core.

export interface User {
  id: string;
  username: string;
  disabled: boolean;
  created_at: number;
}

export interface Segment {
  text: string;
  hit?: boolean;
}

export interface NoteMeta {
  id: string;
  title: string;
  preview: string;
  pinned: boolean;
  trashed_at: number | null;
  created_at: number;
  updated_at: number;
  size_bytes: number;
  sha256: string;
  tag_ids: string[];
  attachment_count: number;
  snippet?: Segment[];
}

export interface Attachment {
  id: string;
  ref: string;
  note_id: string;
  filename: string;
  mime: string;
  size_bytes: number;
  state: 'uploading' | 'ready' | 'deleting';
  created_at: number;
  inline: boolean;
}

export interface Note extends NoteMeta {
  body: string;
  attachments: Attachment[];
}

export interface NoteList {
  items: NoteMeta[];
  next_cursor: string;
}

export interface SaveResult {
  sha256: string;
  updated_at: number;
  title: string;
  preview: string;
  size_bytes: number;
  /** The note's attachments, sent when the save copied images pasted from other notes. */
  attachments?: Attachment[];
}

/** A "Check my notes" run (GET/POST /api/me/doctor). */
export interface DoctorJob {
  running: boolean;
  fix?: boolean;
  started_at?: number;
  finished_at?: number;
  report?: { problems: string[]; fixed: string[] };
  error?: string;
}

export interface Tag {
  id: string;
  name: string;
  color: string;
  count: number;
}

export interface Storage {
  used_bytes: number;
  free_bytes: number;
  min_free_bytes: number;
}

export type Sort = 'updated' | 'created' | 'title';
