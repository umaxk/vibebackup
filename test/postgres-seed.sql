CREATE TABLE IF NOT EXISTS items (
    id         serial PRIMARY KEY,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO items (name) VALUES ('alpha'), ('beta'), ('gamma'), ('delta');
