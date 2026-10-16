-- SPDX-License-Identifier: AGPL-3.0-only
-- Knowledge corpora and role skills (profile knowledge: KNW-1 to KNW-5, SKL-1, SKL-2, D-97).

-- A corpus document: a farm file record (ART-1) of one tenant's corpus. id is the object id of
-- the document in fragment ids (<id>:<n>, kb-read-request); source git is a document of the
-- tenant's knowledge/<corpus>/ folder that came through apply (path and the hash of its git
-- bytes, so an unchanged document is not ingested again), source upload one a person uploaded
-- with kb.write:<corpus> (KNW-4). ingest_task is the kb.ingest.v1 job that cuts it into
-- fragments; only that task writes them (P1 kb.index_put). A deleted document keeps its row
-- without its path as a tombstone, so a late index_put of its task writes nothing. A document
-- whose ingest was refused or ended without an index is failed with the reason, and the next
-- apply enqueues it again (ingest_runs counts the jobs).
CREATE TABLE corpus_documents (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    corpus      text NOT NULL,
    file_ref    text NOT NULL UNIQUE,
    source      text NOT NULL CHECK (source IN ('git', 'upload')),
    path        text,
    git_sha256  text,
    state       text NOT NULL CHECK (state IN ('pending', 'indexed', 'failed', 'deleted')),
    fail_reason text,
    ingest_task text,
    ingest_runs int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE INDEX corpus_documents_corpus ON corpus_documents (tenant, corpus) WHERE state <> 'deleted';
CREATE UNIQUE INDEX corpus_documents_task ON corpus_documents (ingest_task) WHERE ingest_task IS NOT NULL;

-- The per-tenant word index of the corpora in C1 (KNW-1, D-97): a fragment of one document,
-- class content (RET-1). Every read names the tenant, and the words are a tsvector of the
-- 'simple' configuration (lowercased words, no stemming and no stop words). There are no
-- embeddings.
CREATE TABLE corpus_fragments (
    doc     text NOT NULL REFERENCES corpus_documents (id),
    n       int NOT NULL CHECK (n BETWEEN 0 AND 1999),
    tenant  text NOT NULL,
    corpus  text NOT NULL,
    title   text,
    body    text NOT NULL,
    words   tsvector GENERATED ALWAYS AS (to_tsvector('simple', coalesce(title, '') || ' ' || body)) STORED,
    PRIMARY KEY (doc, n)
);
CREATE INDEX corpus_fragments_words ON corpus_fragments USING gin (words);
CREATE INDEX corpus_fragments_corpus ON corpus_fragments (tenant, corpus);

-- A document's fragments go together with its file (KNW-5, RET-10): whatever moves the file out
-- of stored (the expiry of the retention loop, an erasure request, the deletion of a document
-- or a corpus at apply, which go through an erasure request too) deletes the fragments in the
-- same transaction and leaves the document a tombstone without its path.
CREATE FUNCTION corpus_file_gone() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM corpus_fragments WHERE doc IN (SELECT id FROM corpus_documents WHERE file_ref = NEW.file_ref);
    UPDATE corpus_documents SET state = 'deleted', path = NULL, updated_at = botaru_now() WHERE file_ref = NEW.file_ref;
    RETURN NEW;
END
$$;
CREATE TRIGGER corpus_file_gone AFTER UPDATE OF state ON files
    FOR EACH ROW WHEN (OLD.state = 'stored' AND NEW.state <> 'stored') EXECUTE FUNCTION corpus_file_gone();

-- The core copy of a skill of the farm git (SKL-2): the files of skills/<name>/ by their path,
-- kept by the tree hash of spec/hashing.md as apply received them. A run gets a skill only from
-- this copy and only when the hash equals the role's skill-ref. Skills are farm-wide reviewed
-- text, not tenant data.
CREATE TABLE skill_trees (
    name       text NOT NULL,
    sha256     text NOT NULL,
    files      jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (name, sha256)
);
