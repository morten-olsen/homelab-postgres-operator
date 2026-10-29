# Operator releases

Forgejo (`homelab/postgres-operator`) owns all Git refs. GitHub is a push mirror;
its Actions publish the existing public GHCR image, release assets and Helm
repository. No workflow calculates a next version or creates a version tag.
Main/branch merges run checks only. Publication needs Morten's explicit approval
for the repository, version, exact commit and publishing/deployment effects.

## Prepare and publish

1. Fetch Forgejo main and all tags. Wait for the previous publication to finish.
   Choose a canonical `vMAJOR.MINOR.PATCH` newer than **both** all source tags
   and the published chart history. Chart metadata in the checkout is not a
   reliable next-version counter: releases stamp version/appVersion from the tag.
2. Run this read-only preflight at the intended full main commit (requires Git,
   Python 3 and Helm 3):

   ```sh
   python3 scripts/release_check.py --operator postgres-operator \
     --tag vMAJOR.MINOR.PATCH --commit FULL_SHA --plan
   ```

   It checks the existing public chart repository with a fresh Helm cache,
   GHCR manifest and release metadata. Only HTTP 404 means absent; outages,
   forbidden responses, empty chart history and malformed versions stop it.
   A plan never creates a tag or authorizes publication.
3. Obtain Morten's approval. Recheck the exact commit, green source/mirror CI
   and unused version. Create and push that one tag to **Forgejo only**, without
   force. Never move, reuse or delete a version tag, even after a failed build.
4. Inspect `tea api repos/homelab/postgres-operator/push_mirrors` and read-only
   `git ls-remote` on both sides. The mirrored tag must peel to the approved SHA.
   Wait for Release Safety Tests, Verify Forgejo Release, image build, release
   assets and Helm Pages publication to succeed at that exact commit.
5. Verify published version/appVersion, image revision labels and digest,
   historical chart availability, and source/mirror tags after another mirror
   sync. A deployment change is separate GitOps work: verify ArgoCD, actual
   objects and the original symptom when that approved rollout happens.

The publication gate requires the canonical tag in Forgejo at the exact event
commit, that commit on authoritative main, and a version newer than every other
source reservation and every published chart. Existing images or release
metadata block partial-publication retries, even when the source tag is missing.
Each publisher rechecks source/version and its own artifact immediately before
writing, so retrying an individual failed job cannot bypass the original gate.
Image → release assets → Pages run in order. Pages follows a successful Release
through `workflow_run` on the default branch: the existing `github-pages`
environment allows main only. A separate read-only job runs the trusted main
verifier, rejects foreign/PR/failed/malformed event metadata, and proves the
authoritative tag/SHA, ancestry and prior artifacts before passing validated
outputs to the Pages job. It never executes event-supplied code with publishing
permissions before that check, downloads no workflow artifacts and adds no
credential, environment policy or token scope. The build cannot bypass a failed
gate on a tag event. Publications serialize;
push one approved tag and wait before approving/pushing another. No new
credential or permission is needed; source checks use public read endpoints.

## Recovery and rollback

Do not rerun a partially published version or restore an old missing tag without
an explicit recovery approval. A tag restored at an old commit uses **that
commit's old workflow** and can rebuild/overwrite an existing image. Record the
original release run, source SHA, published chart and image digests before
asking. Recovering a ref does not justify inventing a source commit or changing
an artifact. HTTP/API failures must be investigated, never treated as absence.

Reverting this workflow restores unsafe main-triggered downstream tag creation
and automatic publication; that revert itself needs release approval. Keep the
source-owned tag flow during recovery. Published artifacts and pushed tags
cannot be undone by reverting code. Roll back a workload with its recorded
previous digest through GitOps, under the relevant approval boundaries.

Run the isolated safety regression suite with:

```sh
python3 -m unittest discover -s scripts -p 'test_release_check.py' -v
```

It includes a local bare-repository reproduction: `git push --mirror` removes a
GitHub-only tag; the same tag originating in Forgejo survives subsequent syncs.
The workflow and this runbook must change together.
