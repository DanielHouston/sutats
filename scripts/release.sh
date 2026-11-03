#!/usr/bin/env bash
# Publish a release to the public repository.
#
#   scripts/release.sh 1.0.1            release v1.0.1
#   scripts/release.sh 1.0.1 --dry-run  show what would happen, push nothing
#
# Development happens on the development remote with its full history. The
# public repository gets one commit per release, holding the released tree,
# on top of its previous release, so its history is a list of releases:
#
#   1. checks the working tree is clean, HEAD is the source branch and is
#      pushed to the development remote, and the version is new and higher
#   2. runs go vet and the tests
#   3. refuses to publish if the tree, the release notes or the public
#      name and email contain any pattern from the private patterns file
#      (one extended regex per line; it lives inside .git so it is never
#      published itself)
#   4. commits the tree on top of the public branch under the public name
#      and email, with the commit titles since the last release as notes
#   5. pushes that commit and an annotated tag vX.Y.Z to the public remote,
#      which makes CI build and publish the release images, and creates a
#      GitHub release when the gh CLI is logged in
#   6. tags the released source commit vX.Y.Z on the development remote
#
# The release commit and both tags are signed when git has a signing key
# (git config user.signingkey), so the public host can show them verified.
# GPG asks for the key's passphrase once; it is checked before anything is
# pushed.
#
# Settings, from the environment:
#   SOURCE_BRANCH   branch to release                    (master)
#   DEV_REMOTE      development remote                   (origin)
#   PUBLIC_REMOTE   public remote                        (github)
#   PUBLIC_BRANCH   branch on the public remote          (main)
#   FORBID_FILE     private patterns file                (.git/release-forbid)
#   SKIP_TESTS=1    do not run the tests
#   NO_GH_RELEASE=1 push the tag but create no GitHub release
#   SIGN=0          do not sign, even with a signing key configured
#   PUBLIC_NAME     name on the public commit and tag    (git config
#                   release.publicName, else user.name)
#   PUBLIC_EMAIL    email on the public commit and tag   (git config
#                   release.publicEmail, else user.email), such as a
#                   GitHub noreply address; the signing key needs it as a
#                   uid for the host to show the signatures as verified
#
# Tags exist under the same name on both remotes but point at different
# commits, so the public remote must not fetch tags:
#   git config remote.<public remote>.tagOpt --no-tags
set -euo pipefail

SOURCE_BRANCH=${SOURCE_BRANCH:-master}
DEV_REMOTE=${DEV_REMOTE:-origin}
PUBLIC_REMOTE=${PUBLIC_REMOTE:-github}
PUBLIC_BRANCH=${PUBLIC_BRANCH:-main}

die() { echo "release: $*" >&2; exit 1; }
say() { echo "release: $*"; }

version=${1:-}
dry=false
[[ ${2:-} == --dry-run || ${1:-} == --dry-run ]] && dry=true
[[ ${1:-} == --dry-run ]] && version=${2:-}
version=${version#v}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
	die "usage: scripts/release.sh <version> [--dry-run], with a version like 1.2.3"
tag=v$version

cd "$(git rev-parse --show-toplevel)"
FORBID_FILE=${FORBID_FILE:-$(git rev-parse --git-dir)/release-forbid}
run() { if $dry; then echo "  would run: $*"; else "$@"; fi; }

# ---- 1. preconditions ----
git remote get-url "$DEV_REMOTE" >/dev/null 2>&1 || die "no remote called $DEV_REMOTE"
git remote get-url "$PUBLIC_REMOTE" >/dev/null 2>&1 || die "no remote called $PUBLIC_REMOTE"
[[ $(git config "remote.$PUBLIC_REMOTE.tagOpt" || true) == --no-tags ]] ||
	die "run: git config remote.$PUBLIC_REMOTE.tagOpt --no-tags (its tags would clash with the development ones)"
[[ -z $(git status --porcelain) ]] || die "the working tree has uncommitted changes"
[[ $(git symbolic-ref --short -q HEAD || true) == "$SOURCE_BRANCH" ]] || die "check out $SOURCE_BRANCH first"

git fetch -q "$DEV_REMOTE" "$SOURCE_BRANCH"
git fetch -q --no-tags "$PUBLIC_REMOTE" || true
src=$(git rev-parse HEAD)
[[ $src == "$(git rev-parse "$DEV_REMOTE/$SOURCE_BRANCH")" ]] ||
	die "$SOURCE_BRANCH differs from $DEV_REMOTE/$SOURCE_BRANCH; push or pull first"

# Who the public commit and tag are from. The development tag in step 6
# keeps the ordinary git identity.
pub_name=${PUBLIC_NAME:-$(git config release.publicName || git config user.name || true)}
pub_email=${PUBLIC_EMAIL:-$(git config release.publicEmail || git config user.email || true)}
[[ -n $pub_name && -n $pub_email ]] ||
	die "no name or email for the public commit; set PUBLIC_NAME/PUBLIC_EMAIL or git config release.publicName/release.publicEmail"

git rev-parse -q --verify "refs/tags/$tag" >/dev/null && die "$tag already exists here"
git ls-remote --exit-code --tags "$DEV_REMOTE" "refs/tags/$tag" >/dev/null && die "$tag already exists on $DEV_REMOTE"
git ls-remote --exit-code --tags "$PUBLIC_REMOTE" "refs/tags/$tag" >/dev/null && die "$tag already exists on $PUBLIC_REMOTE"

# Signing: on whenever a key is configured, unless SIGN=0.
signkey=$(git config user.signingkey || true)
sign=false
[[ -n $signkey && ${SIGN:-} != 0 ]] && sign=true
gpg=$(git config gpg.program || echo gpg)
if $sign; then
	# GPG's passphrase prompt needs to know the terminal.
	if t=$(tty 2>/dev/null); then export GPG_TTY=$t; fi
	if $dry; then
		say "would sign with $signkey"
	else
		say "checking the signing key $signkey"
		echo "release check" | "$gpg" --local-user "$signkey" --sign >/dev/null ||
			die "cannot sign with $signkey; unlock it first: echo test | gpg --local-user '$signkey' --clearsign"
	fi
fi

prev=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$src" 2>/dev/null || true)
if [[ -n $prev ]]; then
	highest=$(printf '%s\n%s\n' "${prev#v}" "$version" | sort -V | tail -1)
	[[ $highest == "$version" && ${prev#v} != "$version" ]] || die "$tag is not higher than the last release, $prev"
fi

# ---- 2. tests ----
if [[ ${SKIP_TESTS:-} != 1 ]]; then
	say "running go vet and the tests"
	go vet ./...
	go test ./... >/dev/null || die "tests failed"
fi

# ---- 3. release notes and the private scan ----
range=${prev:+$prev..}$src
# Drop CI directives: a release commit carrying [skip ci] would skip the
# very build that publishes it.
notes=$(git log --no-merges --format='- %s' "$range" |
	sed -E 's/ *\[(skip ci|ci skip|no ci|skip actions|actions skip)\]//g')
[[ -n $notes ]] || notes="- No changes since $prev."
message="Release $tag

$notes"

if [[ -f $FORBID_FILE ]]; then
	patterns=$(grep -v -E '^\s*(#|$)' "$FORBID_FILE" || true)
	if [[ -n $patterns ]]; then
		leaks=$(git grep -n -I -E -f <(echo "$patterns") "$src" -- . || true)
		msgleaks=$(echo "$message" | grep -n -E -f <(echo "$patterns") || true)
		identleak=$(echo "$pub_name <$pub_email>" | grep -E -f <(echo "$patterns") || true)
		if [[ -n $leaks || -n $msgleaks || -n $identleak ]]; then
			found=$(printf '%s\n' "$leaks" "${msgleaks:+release notes: $msgleaks}" \
				"${identleak:+as the author and tagger: $identleak (set release.publicEmail)}" | grep -v '^$')
			die "private details would be published:"$'\n'"$found"
		fi
		say "private scan clean ($(echo "$patterns" | wc -l) patterns)"
	fi
else
	say "no private patterns file at $FORBID_FILE; skipping the scan"
fi

# ---- 4. the public commit ----
say "publishing as $pub_name <$pub_email>"
export GIT_AUTHOR_NAME=$pub_name GIT_AUTHOR_EMAIL=$pub_email GIT_COMMITTER_NAME=$pub_name GIT_COMMITTER_EMAIL=$pub_email
tree=$(git rev-parse "$src^{tree}")
parent=$(git rev-parse -q --verify "refs/remotes/$PUBLIC_REMOTE/$PUBLIC_BRANCH" || true)
if [[ -n $parent && $(git rev-parse "$parent^{tree}") == "$tree" ]]; then
	commit=$parent
	say "$PUBLIC_REMOTE/$PUBLIC_BRANCH already has this tree; tagging it as it is"
else
	signflag=()
	if $sign && ! $dry; then signflag=(-S); fi
	commit=$(git commit-tree "${signflag[@]}" "$tree" ${parent:+-p "$parent"} -m "$message")
	if [[ -n $parent ]]; then
		say "release commit ${commit:0:7}, on top of ${parent:0:7}"
	else
		say "release commit ${commit:0:7}, starting the public history"
	fi
fi
# The tag object is built by hand because it must not exist as a local tag
# (the source commit gets that name). A signed tag is the tag text followed
# by a detached signature of it, which is what git tag -s writes.
tagfile=$(mktemp)
trap 'rm -f "$tagfile" "$tagfile.sig"' EXIT
printf 'object %s\ntype commit\ntag %s\ntagger %s\n\n%s\n' \
	"$commit" "$tag" "$(git var GIT_COMMITTER_IDENT)" "$message" >"$tagfile"
if $sign && ! $dry; then
	"$gpg" --local-user "$signkey" --armor --detach-sign --output "$tagfile.sig" "$tagfile"
	cat "$tagfile.sig" >>"$tagfile"
fi
tagobj=$(git mktag <"$tagfile")
unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

echo
echo "$message"
echo

# ---- 5. publish ----
run git push "$PUBLIC_REMOTE" "$commit:refs/heads/$PUBLIC_BRANCH" "$tagobj:refs/tags/$tag"
url=$(git remote get-url "$PUBLIC_REMOTE")
if [[ ${NO_GH_RELEASE:-} != 1 && $url == *github.com* ]] && command -v gh >/dev/null && gh auth status >/dev/null 2>&1; then
	repo=$(echo "$url" | sed -E 's#^(https://github\.com/|git@github\.com:)##; s#\.git$##')
	run gh release create "$tag" --repo "$repo" --title "$tag" --notes "$notes" --verify-tag
else
	say "no GitHub release created (gh missing, not logged in, or NO_GH_RELEASE=1)"
fi

# ---- 6. mark the source commit ----
if $sign; then tagflag=-s; else tagflag=-a; fi
run git tag "$tagflag" "$tag" "$src" -m "Release $tag"
run git push "$DEV_REMOTE" "refs/tags/$tag"

if $dry; then say "dry run: nothing was pushed"; else say "released $tag"; fi
