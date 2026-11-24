package store

// dependencyReadinessSQL defines the shared dispatch and wait predicate.
// Changes to this function require a new migration for already upgraded databases.
const dependencyReadinessSQL = `CREATE OR REPLACE FUNCTION dependency_pair_ready(p jobs, j jobs) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT COALESCE((p.status='merged'
 OR (p.status IN ('closed','failed') AND jsonb_typeof(j.view->'dependencyMerges'->(p.id::text))='object'
 AND jsonb_typeof(j.view->'dependencyMerges'->(p.id::text)->'pr')='number'
 AND (j.view->'dependencyMerges'->(p.id::text)->>'pr') ~ '^[1-9][0-9]*$'
 AND jsonb_typeof(j.view->'dependencyMerges'->(p.id::text)->'mergeCommit')='string'
 AND (j.view->'dependencyMerges'->(p.id::text)->>'mergeCommit') ~ '^[a-f0-9]{40}$'
 AND ((p.status='closed' AND COALESCE(j.view->'dependencyMerges'->(p.id::text)->>'source','')<>'rejected_report') OR (
   j.view->'dependencyMerges'->(p.id::text)->>'source'='rejected_report'
   AND p.view#>>'{continuation,artifact,generation}'=p.generation::text
   AND j.view->'dependencyMerges'->(p.id::text)->>'pr'=p.pr::text
   AND j.view->'dependencyMerges'->(p.id::text)->>'generation'=p.generation::text
   AND j.view->'dependencyMerges'->(p.id::text)->>'head'=p.view#>>'{continuation,artifact,headSha}'
   AND j.view->'dependencyMerges'->(p.id::text)->>'branch'=p.view#>>'{continuation,artifact,branch}'
 )))
 OR (p.status='closed' AND (
 (jsonb_typeof(j.view->'acceptanceTransferWait'->'ownedTargets')='object' AND (j.view->'acceptanceTransferWait'->'ownedTargets') ? p.id::text)
 OR (jsonb_typeof(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text))='object'
 AND j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'relation'='unrelated'
 AND jsonb_typeof(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->'evidence')='string'
 AND (j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'evidence') ~ '[^[:space:]]'
 AND char_length(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'evidence')
   + char_length(regexp_replace(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'evidence', '[^𐀀-􏿿]', '', 'g')) <= 4000
 AND jsonb_typeof(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->'consultationId')='string'
 AND (j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'consultationId') ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
 AND replace(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'consultationId', '-', '') = left(encode(sha256(convert_to(j.id::text||':'||lower(p.repo)||'#'||p.issue::text, 'UTF8')), 'hex'), 32)
 AND jsonb_typeof(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->'at')='string'
 AND (j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'at') ~ '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$'
 AND pg_input_is_valid(j.view->'dependencyReviews'->(lower(p.repo)||'#'||p.issue::text)->>'at', 'timestamp with time zone')
 )))), false)
$$;
`
