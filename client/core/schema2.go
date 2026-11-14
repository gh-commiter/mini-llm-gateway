package core

import "database/sql"

// Migration 2: the policy tables. Nothing is deleted with a cascade: users are deactivated, projects archived.
func migrate2(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE users(id INTEGER PRIMARY KEY, name TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1, owner INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX users_name ON users(name COLLATE NOCASE);
CREATE TABLE device_bindings(device_id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id));
CREATE INDEX device_bindings_user ON device_bindings(user_id);
CREATE TABLE projects(id INTEGER PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
CREATE TABLE memberships(project_id INTEGER NOT NULL REFERENCES projects(id), user_id INTEGER NOT NULL REFERENCES users(id),
  role TEXT NOT NULL CHECK(role IN('reader','developer','admin')), PRIMARY KEY(project_id,user_id));
CREATE INDEX memberships_user ON memberships(user_id);
CREATE TABLE grants(source_id INTEGER NOT NULL REFERENCES projects(id), reader_id INTEGER NOT NULL REFERENCES projects(id),
  class TEXT NOT NULL CHECK(class IN('structure','data')), PRIMARY KEY(source_id,reader_id,class));
CREATE INDEX grants_reader ON grants(reader_id);
CREATE TABLE notes(id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL REFERENCES projects(id),
  class TEXT NOT NULL CHECK(class IN('structure','data','private')), title TEXT NOT NULL, body TEXT NOT NULL,
  bytes INTEGER NOT NULL, author_id INTEGER NOT NULL REFERENCES users(id),
  created INTEGER NOT NULL, updated INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 1);
CREATE INDEX notes_list ON notes(project_id, updated DESC, id);
CREATE TABLE idempotency(user_id INTEGER NOT NULL, project_id INTEGER NOT NULL, key TEXT NOT NULL, hash TEXT NOT NULL,
  note_id INTEGER NOT NULL, result TEXT NOT NULL, time INTEGER NOT NULL, PRIMARY KEY(user_id,project_id,key));
CREATE INDEX idempotency_time ON idempotency(time);`)
	return err
}
