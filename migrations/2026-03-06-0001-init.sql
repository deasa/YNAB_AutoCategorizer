CREATE TABLE IF NOT EXISTS searchable_categories (
    category TEXT NOT NULL,
    description TEXT NOT NULL,
    content TEXT NOT NULL,
    full_emb F32_BLOB(768) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS emb_idx ON searchable_categories (libsql_vector_idx(full_emb));