# Operator releases

Forgejo (`homelab/postgres-operator`) owns all Git refs. Push approved version tags
there; the existing authenticated push mirror carries them to GitHub, where
Actions publish the existing GHCR image, release assets and Helm repository.
Main/branch merges run checks only. No workflow calculates a next version or
creates a version tag. Publication needs Morten's explicit approval for the
repository, version, exact source commit and publishing/deployment effects.

## Prepare and publish

1. Fetch Forgejo main and all tags. Wait for the previous publication to finish.
   Choose a canonical `vMAJOR.MINOR.PATCH` newer than **both** all source tags
   and the published chart history. Checkout chart metadata is not a reliable
   next-version counter: publishers stamp version/appVersion from the tag.
2. Run this read-only preflight at the intended full main commit (Git, Python 3
   and Helm 3 required):

   ```sh
   python3 scripts/release_check.py --operator postgres-operator \
     --tag vMAJOR.MINOR.PATCH --commit FULL_SHA --plan
   ```

   A local plan reads authoritative Forgejo refs, fresh public chart history,
   GHCR manifests and release metadata. Only HTTP 404 means absent; outages,
   forbidden responses, empty chart history and malformed versions stop it.
   A plan never creates a tag or exports publication outputs.
3. Obtain Morten's approval for the exact version/commit and effects. Recheck
   that candidate and its green build. Release Safety Tests run the following
   read-only probe on the actual hosted runner before any publication:

   ```sh
   python3 scripts/release_check.py --operator postgres-operator --check-source
   ```

   It verifies that GitHub can read its public mirror's current main, with no
   artifact lookup or publishing. The hosted runner never connects back to
   private Forgejo. No VPN, inbound route, credential or permission is added.
4. Push that one tag to **Forgejo only**, without force. Inspect
   `tea api repos/homelab/postgres-operator/push_mirrors` and read both remotes with
   `git ls-remote`. The source and mirrored tag must peel to the approved SHA.
   Wait for Release Safety Tests, Verify Forgejo Release, image build, release
   assets and Helm Pages publication to succeed at that exact source commit.
5. Verify published version/appVersion, both image architectures and revision
   labels/digests, historical chart bytes and source/mirror tags after another
   mirror sync. A deployment change is separate GitOps work: verify ArgoCD,
   actual objects and the original symptom when an approved rollout happens.

## Publication checks

The hosted gate reads the fixed public GitHub mirror URL. It requires the exact
mirrored tag/event SHA and ancestry in mirrored main, then compares the version
against every other stable tag and the fresh published chart history. This
uses the existing Forgejo push mirror as the delivery trust channel: Morten is
the sole user on both forges, and tag operations happen in Forgejo. The owner
checks authoritative source/mirror equality from inside the home network;
GitHub does not query a live private source endpoint or accept a caller-supplied
Git URL. Mirror failures are repaired in Forgejo, never with downstream refs.

A consumed version is refused even when its tag is missing. Existing images
and release metadata block partial-publication retries. Each publisher repeats
its source/version/artifact checks immediately before writing, including
individual job retries. Lookup errors fail closed. All checkouts performing
ancestry checks fetch full history. Image, release assets and Pages run in
order; push one approved tag and wait for the complete publication.

Pages follows a successful Release through `workflow_run` on the default
branch, preserving the existing main-only `github-pages` environment. Its
read-only verifier runs trusted default-branch code, rejects foreign, PR,
failed or malformed event metadata, and checks the mirrored tag/SHA, ancestry
and completed image/release before exporting the release checkout. It uses no
workflow artifacts or executable outputs as a handoff. Publishing permissions
remain isolated after validation; no token scope or environment policy changes.

## Recovery and rollback

Do not move, reuse or delete a version tag, even after a failed build. Do not
rerun a partially published version or restore a missing historical tag without
explicit recovery approval. A tag at an old commit executes **that commit's
old workflow** and can rebuild/overwrite an existing image after main is fixed.
Record the source SHA, successful original run and chart/image digests before
asking. Recovering a ref does not authorize changing its artifacts.

Reverting this workflow restores unsafe downstream tag creation or the
unreachable private-source check; keep the outward tag flow during recovery.
Published artifacts and pushed tags are lasting. Roll back a workload through
GitOps to its recorded previous digest under the applicable approval boundaries.

Run the isolated safety regression suite with:

```sh
python3 -m unittest discover -s scripts -p 'test_release*.py' -v
```

It includes real temporary Git mirrors: downstream-only tags are pruned,
source-owned tags survive, and hosted verification uses only the outward
mirror while rejecting missing/wrong tags, unmerged commits and reused versions.
Update this runbook in the same commit as workflow changes.

HOMEIT-434's approved `v0.0.21` at `c19f58b` is retained as an unpublished
reservation. Release 40943734697 stopped at the old private-source lookup before
any artifact publication. Never move/delete it; the repaired workflow needs a
new exact version/commit publication decision.
