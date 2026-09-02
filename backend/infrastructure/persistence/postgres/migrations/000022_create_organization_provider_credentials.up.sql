-- Credentials for the single active external organization-data provider.
-- Singleton row: exactly one provider is active at a time, matching the
-- organization snapshot contract (docs/organization-snapshot-contract.md).
-- api_token_encrypted holds ciphertext only; plaintext is never persisted.
CREATE TABLE IF NOT EXISTS organization_provider_credentials (
    id INTEGER PRIMARY KEY DEFAULT 1 CONSTRAINT singleton_provider CHECK (id = 1),
    provider VARCHAR(50) NOT NULL DEFAULT 'podiq',
    api_token_encrypted TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
