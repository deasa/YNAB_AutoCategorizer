CREATE TABLE IF NOT EXISTS learned_payees_v2 (
    payee_name TEXT NOT NULL,
    category TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(payee_name, category)
);

INSERT OR IGNORE INTO learned_payees_v2 (payee_name, category, created_at)
    SELECT payee_name, category, created_at FROM learned_payees;

DROP TABLE IF EXISTS learned_payees;

ALTER TABLE learned_payees_v2 RENAME TO learned_payees;
