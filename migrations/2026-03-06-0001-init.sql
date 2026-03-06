CREATE TABLE searchable_categories (
                                    category TEXT NOT NULL,
                                    description text NOT NULL,
                                    content TEXT NOT NULL,
                                    full_emb F32_BLOB(1536) NOT NULL,
                                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX emb_idx ON searchable_categories (libsql_vector_idx(full_emb));