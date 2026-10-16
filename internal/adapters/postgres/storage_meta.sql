-- Reference fragment: storage_meta (C9). Applied once, by the first migration of
-- a service, through `-- umir:include storage_meta`.
--
-- One row names the storage generation. It is created here with
-- gen_random_uuid() and changes only when the stopped service is rotated by the
-- owner role (`<bin> generation rotate --reason <text>`), for example after a
-- restore from backup. The runtime role needs SELECT only.
CREATE TABLE storage_meta (
    singleton           boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    generation          uuid        NOT NULL DEFAULT gen_random_uuid(),
    previous_generation uuid,
    rotated_at          timestamptz NOT NULL DEFAULT now(),
    rotation_reason     text        NOT NULL DEFAULT 'initial'
);

INSERT INTO storage_meta (singleton) VALUES (true);
