-- Reference fragment: inbox (C4). The primary key makes a message applied at most
-- once per consumer; the record and the effect share one transaction.
CREATE TABLE inbox (
    consumer    text        NOT NULL,
    message_id  text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, message_id)
);

CREATE INDEX inbox_received_idx ON inbox (received_at);
