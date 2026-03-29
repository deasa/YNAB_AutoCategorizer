CREATE TABLE IF NOT EXISTS learned_payees (
    payee_name TEXT NOT NULL,
    category TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(payee_name)
);
