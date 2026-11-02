#!/usr/bin/env bash
# Steps of the DriftGate GitHub Action (action.yml). Every step is one
# function; action.yml calls `driftgate.sh <step>` with the inputs in the
# environment (INPUT_*). The steps never fail on a DriftGate result: the exit
# code is kept in $DG_OUT/exit-code, reports and comments are made from it,
# and only the last step (finish) fails the job.
set -euo pipefail

: "${RUNNER_TEMP:=${TMPDIR:-/tmp}}"
DG_OUT="${DG_OUT:-$RUNNER_TEMP/driftgate}"
mkdir -p "$DG_OUT"
LOG="$DG_OUT/run.log"
CODE_FILE="$DG_OUT/exit-code"

# out sets a step output; the delimiter form keeps a value with a newline in
# it from setting other outputs.
out() {
	if [ -z "${GITHUB_OUTPUT:-}" ]; then
		echo "output $1=$2"
		return 0
	fi
	local d
	d="driftgate_$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	printf '%s<<%s\n%s\n%s\n' "$1" "$d" "$2" "$d" >>"$GITHUB_OUTPUT"
}
note() { echo "$*" | tee -a "$LOG"; }

# stop records the exit code of the action and why; later steps skip the run.
stop() {
	echo "$1" >"$CODE_FILE"
	note "driftgate action: $2"
	echo "$2" >"$DG_OUT/stop-reason"
}
stopped() { [ -f "$CODE_FILE" ]; }

workdir() { cd "${INPUT_WORKING_DIRECTORY:-.}"; }

# env.sh holds what every later step needs: the store and the key.
load_env() {
	# shellcheck disable=SC1091
	[ -f "$DG_OUT/env.sh" ] && . "$DG_OUT/env.sh"
	return 0
}

# install always puts the binary here, whatever else is on the PATH.
dg() { "$RUNNER_TEMP/driftgate-bin/driftgate" "$@"; }

# install puts a driftgate binary on the PATH: an existing one (binary), one
# built from the action's own source (version: source), or a release asset.
step_install() {
	# A failed install leaves exit 3 (environment) for the report and finish
	# steps; an earlier use of the action in this job leaves nothing.
	rm -f "$CODE_FILE" "$DG_OUT/stop-reason" "$LOG" "$DG_OUT/report.md" "$DG_OUT/junit.xml" "$DG_OUT/incomplete"
	echo 3 >"$CODE_FILE"
	echo "driftgate could not be installed" >"$DG_OUT/stop-reason"
	local bin="$RUNNER_TEMP/driftgate-bin"
	mkdir -p "$bin"
	if [ -n "${INPUT_BINARY:-}" ]; then
		local b=""
		b="$(cd "${INPUT_WORKING_DIRECTORY:-.}" && cd "$(dirname "$INPUT_BINARY")" 2>/dev/null && pwd)/$(basename "$INPUT_BINARY")" || true
		if [ ! -x "$b" ]; then
			stop 2 "binary $INPUT_BINARY is not an executable file"
			exit 2
		fi
		ln -sf "$b" "$bin/driftgate"
	elif [ "${INPUT_VERSION:-source}" = source ]; then
		local commit="${ACTION_REF:-unknown}"
		if git -C "$ACTION_PATH" rev-parse HEAD >/dev/null 2>&1; then
			commit="$(git -C "$ACTION_PATH" rev-parse HEAD)"
		fi
		local pkg=github.com/orangehill/driftgate/internal/cli
		(cd "$ACTION_PATH" && CGO_ENABLED=0 go build -trimpath \
			-ldflags "-s -w -X $pkg.Version=source -X $pkg.Commit=$commit -X $pkg.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
			-o "$bin/driftgate" ./cmd/driftgate)
	else
		local os arch
		case "${RUNNER_OS:-$(uname -s)}" in Linux) os=linux ;; macOS | Darwin) os=darwin ;; *)
			stop 2 "runner OS ${RUNNER_OS:-?} is not supported (Linux, macOS)"
			exit 2
			;;
		esac
		case "${RUNNER_ARCH:-$(uname -m)}" in X64 | x86_64) arch=amd64 ;; ARM64 | arm64 | aarch64) arch=arm64 ;; *)
			stop 2 "runner architecture ${RUNNER_ARCH:-?} is not supported"
			exit 2
			;;
		esac
		local repo="${ACTION_REPOSITORY:-}"
		if [ -z "$repo" ]; then
			# github.action_repository can be empty inside a composite
			# action; a downloaded action sits in _actions/<owner>/<repo>/<ref>.
			case "$ACTION_PATH" in
			*/_actions/*/*/*) repo="$(echo "$ACTION_PATH" | sed -E 's#.*/_actions/([^/]+)/([^/]+)/.*#\1/\2#')" ;;
			*) repo="${GITHUB_REPOSITORY:-}" ;;
			esac
		fi
		release_install "$repo" "$INPUT_VERSION" "driftgate-$os-$arch" "$bin/driftgate"
	fi
	echo "$bin" >>"${GITHUB_PATH:-/dev/null}"
	"$bin/driftgate" version
	out path "$bin/driftgate"
	rm -f "$CODE_FILE" "$DG_OUT/stop-reason" "$LOG"
}

# release_install downloads a release binary with SHA256SUMS and its Sigstore
# bundle, and installs the binary only when the checksum matches and, when
# cosign is on the PATH, the signature names this repository's release
# workflow at that tag. Anything else stops the install (fail closed).
release_install() {
	local repo="$1" version="$2" name="$3" dst="$4"
	local info tag url
	# The version goes to gh as an argument: only a tag or latest, so it
	# cannot be read as an option (--web, --repo=…).
	if [ "$version" != latest ] && ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
		stop 2 "version must be source, latest or a release tag vX.Y.Z, not $version"
		exit 2
	fi
	# The release (latest: the newest one that is not a prerelease) gives the
	# tag and the repository's canonical URL, which the certificate names.
	if [ "$version" = latest ]; then
		info="$(gh release view --repo "$repo" --json tagName,url --jq '.tagName + " " + .url')"
	else
		info="$(gh release view "$version" --repo "$repo" --json tagName,url --jq '.tagName + " " + .url')"
	fi
	tag="${info%% *}" url="${info#* }"
	if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || [[ "$url" != https://*/releases/* ]]; then
		stop 3 "release $version of $repo: unexpected tag or URL ($info)"
		exit 3
	fi
	local dl="$RUNNER_TEMP/driftgate-download"
	rm -rf "$dl"
	mkdir -p "$dl"
	gh release download "$tag" --repo "$repo" --dir "$dl" \
		--pattern "$name" --pattern SHA256SUMS --pattern SHA256SUMS.sigstore.json
	if [ ! -f "$dl/$name" ] || [ ! -f "$dl/SHA256SUMS" ]; then
		stop 3 "release $tag of $repo has no $name or no SHA256SUMS; it cannot be verified"
		exit 3
	fi

	# The signature first: SHA256SUMS is trusted only once it verifies.
	if command -v cosign >/dev/null 2>&1; then
		local identity="${url%/releases/*}/.github/workflows/release.yml@refs/tags/$tag"
		if [ ! -f "$dl/SHA256SUMS.sigstore.json" ]; then
			stop 3 "release $tag of $repo has no SHA256SUMS.sigstore.json; the signature cannot be verified"
			exit 3
		fi
		if ! cosign verify-blob --bundle "$dl/SHA256SUMS.sigstore.json" \
			--certificate-identity "$identity" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com \
			"$dl/SHA256SUMS" >"$dl/cosign.log" 2>&1; then
			cat "$dl/cosign.log"
			stop 3 "the signature of SHA256SUMS in release $tag does not verify for $identity"
			exit 3
		fi
		echo "SHA256SUMS of $tag is signed by $identity"
	else
		echo "::warning title=DriftGate::cosign is not installed: the checksum of driftgate $tag is verified, its signature is not"
	fi

	local want got
	want="$(awk -v f="$name" '$2 == f || $2 == "*" f { print $1 }' "$dl/SHA256SUMS")"
	if ! [[ "$want" =~ ^[0-9a-f]{64}$ ]]; then
		stop 3 "SHA256SUMS of release $tag has no single checksum for $name"
		exit 3
	fi
	got="$(sha256_of "$dl/$name")"
	if [ "$want" != "$got" ]; then
		stop 3 "checksum mismatch for $name of release $tag: SHA256SUMS says $want, the download is $got"
		exit 3
	fi
	echo "$name $got matches SHA256SUMS"
	mv "$dl/$name" "$dst"
	chmod +x "$dst"
	rm -rf "$dl"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# prepare sets up the store and the key, and checks that the baseline is in
# the repository: without it, run would make a new one and pass.
step_prepare() {
	stopped && return 0
	date -u +%Y-%m-%dT%H:%M:%SZ >"$DG_OUT/started"
	workdir
	# The pull request's own commands (seed, hooks, adapters) run with this
	# job's environment.
	case "${GITHUB_EVENT_NAME:-}" in
	pull_request_target)
		echo "::warning title=DriftGate::pull_request_target runs the pull request's commands with the base repository's secrets and a write token; use pull_request"
		;;
	pull_request)
		if [ -n "${DRIFTGATE_KEY:-}" ]; then
			echo "::warning title=DriftGate::DRIFTGATE_KEY is set in a pull_request job: the commands of the pull request (seed, hooks, adapters) can read it; leave it unset and the action makes a key of its own"
		fi
		;;
	esac
	local store="${INPUT_STORE:-}" key=""
	if [ -z "${DRIFTGATE_KEY:-}" ] && [ -z "$store" ]; then
		# The store lives only as long as the job, so a key of its own is
		# enough to encrypt the private bodies captured here. The private
		# bodies of the baseline are never on the runner (they stay on the
		# machine that captured them): their requests are skipped, and the
		# result is incomplete unless args has --skip-private.
		key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
		echo "::add-mask::$key"
	fi
	{
		if [ -n "$store" ]; then
			echo "export DRIFTGATE_STORE=$(printf %q "$store")"
		else
			# A store of its own for every job: no stale last comparison, no
			# pristine fingerprints from another run.
			rm -rf "$DG_OUT/store"
			echo "export DRIFTGATE_STORE=$(printf %q "$DG_OUT/store")"
		fi
		[ -n "$key" ] && echo "export DRIFTGATE_KEY=$key"
	} >"$DG_OUT/env.sh"
	chmod 600 "$DG_OUT/env.sh"

	local cfg="${INPUT_CONFIG:-driftgate.yaml}"
	if [ ! -f "$cfg" ]; then
		stop 2 "configuration $cfg not found (working directory $(pwd))"
		return 0
	fi
	if [ "${INPUT_REQUIRE_BASELINE:-true}" = true ]; then
		local b
		b="$(dirname "$cfg")/driftgate/baselines/${INPUT_BASELINE:-local}/responses"
		if [ ! -d "$b" ]; then
			stop 2 "there is no baseline ${INPUT_BASELINE:-local} ($b): make it locally with driftgate run and commit driftgate/; in CI a missing baseline would be made from the code under test and pass"
			return 0
		fi
	fi
}

# seed makes the pristine copies, from the cache when there is one.
step_seed() {
	stopped && return 0
	load_env
	workdir
	local cfg="${INPUT_CONFIG:-driftgate.yaml}"
	if [ -z "${INPUT_PRISTINE_DUMP:-}" ] || [ -z "${INPUT_PRISTINE_LOAD:-}" ]; then
		stop 2 "pristine-cache-files needs pristine-dump and pristine-load commands"
		return 0
	fi
	if [ -z "${CACHE_FILES_HASH:-}" ]; then
		# hashFiles found nothing: the key would never change.
		stop 2 "pristine-cache-files matches no file (the globs are relative to the repository root)"
		return 0
	fi
	export DRIFTGATE_PRISTINE_DIR="$PRISTINE_DIR"
	mkdir -p "$DRIFTGATE_PRISTINE_DIR"
	if [ "${CACHE_HIT:-false}" = true ]; then
		note "pristine cache hit ($CACHE_KEY): loading the dump"
		if env -u DRIFTGATE_KEY bash -eo pipefail -c "$INPUT_PRISTINE_LOAD" >>"$LOG" 2>&1; then
			local code=0
			dg seed --config "$cfg" --skip-commands >"$DG_OUT/seed.out" 2>&1 || code=$?
			cat "$DG_OUT/seed.out" >>"$LOG"
			if [ $code = 0 ]; then
				out seeded cache
				return 0
			fi
			if [ $code = 2 ] && grep -q "skip-commands" "$DG_OUT/seed.out"; then
				echo "::warning title=DriftGate::this driftgate has no 'seed --skip-commands'; the cached dump is not used and the seed runs in full"
			else
				echo "::warning title=DriftGate::the pristine copy from the cache failed (exit $code); the seed runs in full"
			fi
		else
			echo "::warning title=DriftGate::pristine-load failed; the seed runs in full"
		fi
	fi
	local code=0
	note "== seed =="
	dg seed --config "$cfg" >>"$LOG" 2>&1 || code=$?
	if [ $code != 0 ]; then
		stop "$code" "driftgate seed failed (exit $code)"
		return 0
	fi
	out seeded full
	if [ "${CACHE_HIT:-false}" != true ]; then
		# The working databases equal their pristine copies right after seed.
		if env -u DRIFTGATE_KEY bash -eo pipefail -c "$INPUT_PRISTINE_DUMP" >>"$LOG" 2>&1 && [ -n "$(ls -A "$DRIFTGATE_PRISTINE_DIR")" ]; then
			out save true
		else
			echo "::warning title=DriftGate::pristine-dump failed or wrote nothing to \$DRIFTGATE_PRISTINE_DIR; nothing is cached"
		fi
	fi
}

step_run() {
	stopped && return 0
	load_env
	workdir
	local code args=()
	# args is split at whitespace, without globbing; quotes are not
	# interpreted, so a quoted argument is refused rather than misread.
	case "${INPUT_ARGS:-}" in *\'* | *\"* | *\\*)
		stop 2 "args: quotes and backslashes are not supported; arguments are separated by whitespace"
		return 0
		;;
	esac
	read -r -d '' -a args <<<"${INPUT_ARGS:-}" || true
	# The output holds text from responses and from the project's commands:
	# GitHub must not read a line of it as a workflow command.
	local token
	token="driftgate-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	echo "::stop-commands::$token"
	set +e
	dg run --config "${INPUT_CONFIG:-driftgate.yaml}" --baseline "${INPUT_BASELINE:-local}" ${args[@]+"${args[@]}"} 2>&1 | tee -a "$LOG"
	code="${PIPESTATUS[0]}"
	set -e
	echo "::$token::"
	echo "$code" >"$CODE_FILE"
}

# report writes the markdown and JUnit reports from the last comparison; when
# the run stopped before one, from the exit code and the end of the log.
step_report() {
	load_env
	local code
	code="$(cat "$CODE_FILE" 2>/dev/null || echo 3)"
	out exit-code "$code"
	local md="$DG_OUT/report.md" xml="$DG_OUT/junit.xml"
	rm -f "$md" "$xml" "$DG_OUT/incomplete"
	(
		workdir
		local cfg="${INPUT_CONFIG:-driftgate.yaml}"
		[ -f "$cfg" ] || exit 1
		fresh_report "$cfg" || exit 1
		# Exit 3 with a report: private requests could not be compared here.
		if [ "$code" = 3 ]; then
			incomplete "$cfg" || exit 1
		elif [ "$code" != 0 ] && [ "$code" != 1 ]; then
			exit 1
		fi
		dg report --config "$cfg" --format markdown >"$md.tmp" 2>>"$LOG" && mv "$md.tmp" "$md"
		dg report --config "$cfg" --format junit >"$xml.tmp" 2>>"$LOG" && mv "$xml.tmp" "$xml"
		exit 0
	) || true
	rm -f "$md.tmp" "$xml.tmp"
	out result "$(result_name "$code")"
	[ -s "$md" ] || fallback_markdown "$code" >"$md"
	[ -s "$xml" ] || fallback_junit "$code" >"$xml"
	if [ -n "${INPUT_JUNIT:-}" ]; then
		local dst
		dst="$(cd "${INPUT_WORKING_DIRECTORY:-.}" && pwd)/$INPUT_JUNIT"
		mkdir -p "$(dirname "$dst")"
		cp "$xml" "$dst"
		out junit "$dst"
	else
		out junit "$xml"
	fi
	out report "$md"
	if [ "${INPUT_SUMMARY:-true}" = true ] && [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		cat "$md" >>"$GITHUB_STEP_SUMMARY"
	fi
}

# fresh_report: the last comparison exists and was made by this job (a store
# given through the store input may hold an older one).
fresh_report() {
	local created
	created="$(dg report --config "$1" --format json 2>/dev/null | jq -r '.created // empty' 2>/dev/null)" || return 1
	[ -z "$created" ] && return 0
	[[ ! "$created" < "$(cat "$DG_OUT/started")" ]]
}

# incomplete: the last comparison skipped private requests whose bodies are
# not on the runner, without --skip-private; their number goes to a file
# the result and finish steps read.
incomplete() {
	local n
	n="$(dg report --config "$1" --format json 2>/dev/null | jq -r 'if .skip_private then 0 else (.private_missing // [] | length) end' 2>/dev/null)" || return 1
	[ -n "$n" ] && [ "$n" -gt 0 ] || return 1
	echo "$n" >"$DG_OUT/incomplete"
}

result_name() {
	case "$1" in 0) echo clean ;; 1) echo drift ;; 2) echo config-error ;; *)
		if [ -f "$DG_OUT/incomplete" ]; then echo incomplete; else echo environment-error; fi
		;;
	esac
}

explain() {
	case "$1" in
	0) echo "no drift" ;;
	1) echo "drift: the responses or the routes differ from the baseline" ;;
	2) echo "configuration error: driftgate.yaml, the catalog or the baseline is not usable; nothing was compared" ;;
	*)
		if [ -f "$DG_OUT/incomplete" ]; then
			echo "incomplete: $(cat "$DG_OUT/incomplete") private requests could not be compared on the runner, because private bodies (personal data) never leave the machine that captured them. Compare them where the bodies are, or use an anonymized database so responses hold no personal data; args: --skip-private accepts a check of the rest"
		else
			echo "environment error: the application, the database or the state could not be prepared, so the result cannot be trusted. This is not a regression in the API"
		fi
		;;
	esac
}

fallback_markdown() {
	local code="$1"
	echo "### DriftGate: $(result_name "$code") (exit $code)"
	echo
	echo "$(explain "$code")."
	if [ -f "$DG_OUT/stop-reason" ]; then
		echo
		echo "> $(cat "$DG_OUT/stop-reason")"
	fi
	if [ "$code" = 0 ] || [ "$code" = 1 ]; then
		echo
		echo "No comparison report is available (\`driftgate report\` failed or the run stopped before compare); see the log."
	fi
	local why
	why="$(reasons)"
	if [ -n "$why" ]; then
		echo
		echo '```'
		# A fence in the log would end the code block.
		# shellcheck disable=SC2016
		echo "$why" | sed 's/```/` ` `/g'
		echo '```'
	fi
	echo
	echo "The whole log (with the output of the seed and pristine commands) is in the workflow artifact, run.log."
}

# reasons are the last lines driftgate and this action wrote to the log
# (driftgate: …, driftgate action: …). The output of the project's own
# commands (seed, pristine-load, pristine-dump) can hold anything, so it stays
# in the log artifact and out of the comment and the job summary.
reasons() {
	[ -s "$LOG" ] || return 0
	grep -E '^driftgate( action)?: ' "$LOG" | tail -n 20 || true
}

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'; }

fallback_junit() {
	local code="$1" msg
	msg="$(explain "$code" | xml_escape)"
	echo '<?xml version="1.0" encoding="UTF-8"?>'
	if [ "$code" = 0 ]; then
		echo '<testsuites name="driftgate" tests="1" failures="0" errors="0"><testsuite name="driftgate" tests="1" failures="0" errors="0"><testcase name="run" classname="driftgate"/></testsuite></testsuites>'
		return
	fi
	local kind=error
	[ "$code" = 1 ] && kind=failure
	local nf=0 ne=1
	[ "$code" = 1 ] && nf=1 && ne=0
	echo "<testsuites name=\"driftgate\" tests=\"1\" failures=\"$nf\" errors=\"$ne\"><testsuite name=\"driftgate\" tests=\"1\" failures=\"$nf\" errors=\"$ne\"><testcase name=\"run\" classname=\"driftgate\">"
	echo "<$kind message=\"exit $code: $msg\"><![CDATA["
	reasons | sed 's/]]>/]] >/g'
	echo "]]></$kind></testcase></testsuite></testsuites>"
}

# comment posts the markdown report on the pull request, or updates the
# comment this action posted before (found by a hidden marker).
step_comment() {
	local mode="${INPUT_COMMENT:-always}"
	[ "$mode" = off ] || [ "$mode" = false ] && return 0
	local pr="${INPUT_PR_NUMBER:-}"
	if [ -z "$pr" ] && [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
		pr="$(jq -r '.pull_request.number // empty' "$GITHUB_EVENT_PATH")"
	fi
	if [ -z "$pr" ]; then
		echo "not a pull request: no comment"
		return 0
	fi
	# The number goes into API paths.
	if ! [[ "$pr" =~ ^[0-9]+$ ]]; then
		echo "::warning title=DriftGate::pr-number must be a number; no comment"
		return 0
	fi
	local code marker body id
	code="$(cat "$CODE_FILE" 2>/dev/null || echo 3)"
	marker="<!-- driftgate-report:${INPUT_COMMENT_ID:-default} -->"
	body="$DG_OUT/comment.md"
	{
		echo "$marker"
		# GitHub refuses comments over 65536 characters.
		head -c 60000 "$DG_OUT/report.md"
		if [ "$(wc -c <"$DG_OUT/report.md")" -gt 60000 ]; then
			echo
			echo "_The report is cut here; the whole report is in the job summary and the artifact._"
		fi
		echo
		echo "<sub>[DriftGate run](${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}) · commit ${PR_HEAD_SHA:-${GITHUB_SHA:-}}</sub>"
	} >"$body"
	# Only a comment of the identity that posts this one is updated, never
	# someone else's that carries the marker. GITHUB_TOKEN cannot read
	# /user; it posts as github-actions[bot].
	local login
	login="$(gh api user --jq .login 2>/dev/null)" || login=""
	[ -n "$login" ] || login="github-actions[bot]"
	if ! id="$(gh api --paginate "repos/$GITHUB_REPOSITORY/issues/$pr/comments" 2>"$DG_OUT/comment.err" |
		jq -rs --arg login "$login" --arg marker "$marker" \
			'[.[][] | select(.user.login == $login and (.body | startswith($marker))) | .id][0] // empty' 2>>"$DG_OUT/comment.err")"; then
		echo "::warning title=DriftGate::cannot read the comments of #$pr: $(cat "$DG_OUT/comment.err")"
		return 0
	fi
	if [ -n "$id" ]; then
		jq -Rs '{body: .}' "$body" | gh api -X PATCH "repos/$GITHUB_REPOSITORY/issues/comments/$id" --input - >/dev/null 2>"$DG_OUT/comment.err" ||
			echo "::warning title=DriftGate::cannot update the comment on #$pr (permissions: pull-requests: write; a pull request from a fork gets a read-only token): $(cat "$DG_OUT/comment.err")"
		return 0
	fi
	if [ "$mode" = changes ] && [ "$code" = 0 ]; then
		echo "clean and no earlier comment: no comment"
		return 0
	fi
	jq -Rs '{body: .}' "$body" | gh api -X POST "repos/$GITHUB_REPOSITORY/issues/$pr/comments" --input - >/dev/null 2>"$DG_OUT/comment.err" ||
		echo "::warning title=DriftGate::cannot comment on #$pr (permissions: pull-requests: write; a pull request from a fork gets a read-only token): $(cat "$DG_OUT/comment.err")"
}

# finish fails the job unless the run was clean, with a message that says
# whether the API regressed or the environment did not work.
step_finish() {
	local code
	code="$(cat "$CODE_FILE" 2>/dev/null || echo 3)"
	case "$code" in
	0) echo "DriftGate: no drift" ;;
	1) echo "::error title=DriftGate: drift::$(explain 1). The report is in the job summary." ;;
	2) echo "::error title=DriftGate: configuration error::$(explain 2)." ;;
	3)
		if [ -f "$DG_OUT/incomplete" ]; then
			echo "::error title=DriftGate: incomplete (not a regression)::$(explain 3)."
		else
			echo "::error title=DriftGate: environment error (not a regression)::$(explain 3)."
		fi
		;;
	*) echo "::error title=DriftGate: environment error (not a regression)::$(explain "$code")." ;;
	esac
	exit "$code"
}

case "${1:-}" in
install | prepare | seed | run | report | comment | finish) "step_$1" ;;
*)
	echo "usage: driftgate.sh install|prepare|seed|run|report|comment|finish" >&2
	exit 2
	;;
esac
