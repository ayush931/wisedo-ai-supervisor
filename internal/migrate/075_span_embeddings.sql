-- WIS-627: semantic retrieval over approved passages. pgvector is created by the superuser at
-- bootstrap (it is not a trusted extension); here it is only found. One embedding per span and
-- embedding model revision: a model change writes new rows and never mixes revisions, and the
-- ranker reads only the current revision's rows. Embeddings carry no authority: every question
-- still re-reads the canonical source (revocation, authority) before a passage is captured.
CREATE EXTENSION IF NOT EXISTS vector SCHEMA public;

CREATE TABLE supervisor.span_embeddings (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,span_id uuid NOT NULL,
 model text NOT NULL CHECK(length(model) BETWEEN 1 AND 256),
 model_revision text NOT NULL CHECK(length(model_revision) BETWEEN 1 AND 256),
 embedding public.vector(1024) NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,site_id,span_id,model,model_revision),
 FOREIGN KEY(tenant_id,site_id) REFERENCES supervisor.sites(tenant_id,id)
);
CREATE INDEX span_embeddings_hnsw ON supervisor.span_embeddings USING hnsw (embedding public.vector_cosine_ops);

ALTER TABLE supervisor.span_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE supervisor.span_embeddings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON supervisor.span_embeddings USING(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid) WITH CHECK(tenant_id=nullif(current_setting('supervisor.tenant_id',true),'')::uuid);
REVOKE ALL ON supervisor.span_embeddings FROM PUBLIC,supervisor_runtime;
GRANT SELECT,INSERT ON supervisor.span_embeddings TO supervisor_runtime;
CREATE TRIGGER span_embeddings_immutable BEFORE UPDATE OR DELETE ON supervisor.span_embeddings FOR EACH ROW EXECUTE FUNCTION supervisor.evidence_immutable();
