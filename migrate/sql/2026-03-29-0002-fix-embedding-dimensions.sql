DROP INDEX IF EXISTS emb_idx;
DROP TABLE IF EXISTS searchable_categories;
CREATE TABLE searchable_categories (
    category TEXT NOT NULL,
    description TEXT NOT NULL,
    content TEXT NOT NULL,
    full_emb F32_BLOB(768) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX emb_idx ON searchable_categories (libsql_vector_idx(full_emb));
