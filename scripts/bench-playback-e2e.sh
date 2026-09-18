#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# bench-playback-e2e.sh — measure the user-perceived playback path end to end
#
# times POST /api/v1/playback/start, first media bytes, a sustained-throughput
# read, seeks in each delivery's own seek mechanism, resume, subtitle delivery,
# and session stop against a live Silo server. Emits a human table on stdout and
# an optional machine-readable JSON report (--json).
#
# Usage:
#   scripts/bench-playback-e2e.sh [--json PATH] [--item FILE_ID] [--subtitle-ordinal N]
#   scripts/bench-playback-e2e.sh [--find-subtitles [N]]
#
#   --json PATH           write the full JSON report to PATH
#   --item FILE_ID        measure exactly this media file id (repeatable runs on the
#                         same item); discovery and SAMPLE_SIZE are skipped
#   --subtitle-ordinal N  measure only the subtitle track at combined ordinal N
#   --throughput-bytes N  sustained read size in bytes (default 8388608 = 8 MiB)
#   --find-subtitles [N]  scan up to N items and report which plans expose subtitle
#                         tracks and their formats; issues no media/subtitle fetches
#   --help                print this help and exit
#
# Settings come from the environment, or from .silo-dev.env when present (the
# same file scripts/silo-dev reads). Never put the API key on the command line.
#
#   SILO_API_KEY        required. Bearer token used for every request.
#   SILO_URL            base URL, e.g. http://host:8090 (SILO_SERVER also read)
#   PROFILE_ID          profile the requests run as
#   MOVIE_LIBRARY_ID    movie library for discovery (default 31)
#   SERIES_LIBRARY_ID   series library for discovery (default 32)
#
# Measurement knobs:
#   SAMPLE_SIZE         items to randomly sample from discovery (0 = all, default 0)
#   SEED                deterministic random seed (default 42)
#   SEEKS               seeks per item using the delivery's seek mechanism (default 5)
#   SUBTITLE_REPEATS    warm subtitle fetches per track after the cold fetch (default 2)
#   SETTLE_MS           pause between cold and warm subtitle fetches (default 300)
#   RANGE_BYTES         bounded media read size (default 262144)
#   THROUGHPUT_BYTES    sustained read size (default 8388608)
#   STALL_MS            a single read attempt slower than this is a stall (default 500)
#   SUBTITLE_TIMEOUT    per-subtitle-request deadline seconds (default 240)
#   SUBTITLE_ORDINALS   subtitle ordinals probed when the plan publishes none (default 8)
#   RESUME_POSITION     seconds to store for the resume check (default 300)
#   CURL_TIMEOUT        per-request curl --max-time seconds (default 60)
#   RUN_ID              stable id reused by every request in this run
#                       (default e2e-<epoch>-<pid>)
#
# Seek mechanisms are chosen by delivery: server_remux_progressive uses the
# stream handler's `?seek=<seconds>` parameter; original_http/direct uses byte
# ranges; HLS deliveries request a segment a random distance ahead. A seek whose
# response is byte-identical to a non-seek read is reported honoured=false.
#
# The report never contains the API key, bearer tokens, or signed URL query
# strings: URLs are printed as scheme://host/path only.
#
# Exit status is non-zero only when the harness itself fails (bad arguments,
# missing credentials, no items, unreadable report). A deployment that rejects
# playback is recorded as a result and still exits 0.
# ──────────────────────────────────────────────────────────────────────────────

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ENV_FILE="${SILO_DEV_ENV_FILE:-$REPO_ROOT/.silo-dev.env}"

usage() {
	cat <<'EOF'
usage: scripts/bench-playback-e2e.sh [--json PATH] [--item FILE_ID] [--subtitle-ordinal N]
       scripts/bench-playback-e2e.sh [--find-subtitles [N]]

Measures the end-to-end playback path (plan/start, time to first media bytes,
sustained throughput, seek, resume, subtitles, stop) against a live Silo server
and prints a table.

  --json PATH           write the full JSON report to PATH
  --item FILE_ID        measure exactly this media file id (skips discovery)
  --subtitle-ordinal N  measure only the subtitle track at combined ordinal N
  --throughput-bytes N  sustained read size in bytes (default 8388608)
  --find-subtitles [N]  scan up to N items for subtitle tracks; HEAD-enumerates
                        ordinals and cold-fetches one text/bitmap track per item
  -h, --help            print this help and exit

Settings come from the environment or .silo-dev.env: SILO_API_KEY (required),
SILO_URL, PROFILE_ID, MOVIE_LIBRARY_ID, SERIES_LIBRARY_ID.

Knobs: SAMPLE_SIZE, SEED, SEEKS, SUBTITLE_REPEATS, SETTLE_MS, RANGE_BYTES,
THROUGHPUT_BYTES, STALL_MS, RESUME_POSITION, CURL_TIMEOUT, RUN_ID. See the
script header for defaults.
EOF
}

JSON_OUT=""
ITEM_FILTER=""
SUBTITLE_ORDINAL=""
THROUGHPUT_BYTES="${THROUGHPUT_BYTES:-8388608}"
FIND_SUBTITLES=0
FIND_SUBTITLES_LIMIT="${FIND_SUBTITLES_LIMIT:-10}"
while [[ $# -gt 0 ]]; do
	case "$1" in
		--json)
			[[ $# -ge 2 ]] || { printf 'bench-playback-e2e: --json requires a path\n' >&2; exit 2; }
			JSON_OUT="$2"
			shift 2
			;;
		--item)
			[[ $# -ge 2 ]] || { printf 'bench-playback-e2e: --item requires a file id\n' >&2; exit 2; }
			ITEM_FILTER="$2"
			shift 2
			;;
		--subtitle-ordinal)
			[[ $# -ge 2 ]] || { printf 'bench-playback-e2e: --subtitle-ordinal requires a number\n' >&2; exit 2; }
			SUBTITLE_ORDINAL="$2"
			shift 2
			;;
		--throughput-bytes)
			[[ $# -ge 2 ]] || { printf 'bench-playback-e2e: --throughput-bytes requires a number\n' >&2; exit 2; }
			THROUGHPUT_BYTES="$2"
			shift 2
			;;
		--find-subtitles)
			FIND_SUBTITLES=1
			if [[ $# -ge 2 && "$2" =~ ^[0-9]+$ ]]; then
				FIND_SUBTITLES_LIMIT="$2"
				shift 2
			else
				shift
			fi
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			printf 'bench-playback-e2e: unknown argument: %s (see --help)\n' "$1" >&2
			exit 2
			;;
	esac
done

# ── Environment ───────────────────────────────────────────────────────────────

if [[ -f "$ENV_FILE" ]]; then
	set -a
	# shellcheck disable=SC1090
	. "$ENV_FILE"
	set +a
fi

die() {
	printf 'bench-playback-e2e: %s\n' "$1" >&2
	exit "${2:-1}"
}

API_KEY="${SILO_API_KEY:-}"
[[ -n "$API_KEY" ]] ||
	die "SILO_API_KEY is not set. Export it or add it to $ENV_FILE (see .silo-dev.env.example)." 2
PROFILE_ID="${PROFILE_ID:-}"
[[ -n "$PROFILE_ID" ]] ||
	die "PROFILE_ID is not set. Export it or add it to $ENV_FILE." 2

SERVER="${SILO_URL:-${SILO_SERVER:-http://localhost:8090}}"
SERVER="${SERVER%/}"
MOVIE_LIBRARY_ID="${MOVIE_LIBRARY_ID:-31}"
SERIES_LIBRARY_ID="${SERIES_LIBRARY_ID:-32}"

SAMPLE_SIZE="${SAMPLE_SIZE:-0}"
SEED="${SEED:-42}"
SEEKS="${SEEKS:-5}"
SUBTITLE_REPEATS="${SUBTITLE_REPEATS:-2}"
SETTLE_MS="${SETTLE_MS:-300}"
RANGE_BYTES="${RANGE_BYTES:-262144}"
THROUGHPUT_BYTES="${THROUGHPUT_BYTES:-8388608}"
STALL_MS="${STALL_MS:-500}"
SUBTITLE_TIMEOUT="${SUBTITLE_TIMEOUT:-240}"
SUBTITLE_ORDINALS="${SUBTITLE_ORDINALS:-8}"
RESUME_POSITION="${RESUME_POSITION:-300}"
CURL_TIMEOUT="${CURL_TIMEOUT:-60}"
RUN_ID="${RUN_ID:-e2e-$(date +%s)-$$}"

for knob in SAMPLE_SIZE SEED SEEKS SUBTITLE_REPEATS SETTLE_MS RANGE_BYTES THROUGHPUT_BYTES STALL_MS SUBTITLE_TIMEOUT SUBTITLE_ORDINALS CURL_TIMEOUT FIND_SUBTITLES_LIMIT; do
	[[ "${!knob}" =~ ^[0-9]+$ ]] || die "$knob must be a non-negative integer (got '${!knob}')." 2
done
[[ "$THROUGHPUT_BYTES" -gt 0 ]] || die "THROUGHPUT_BYTES must be greater than 0." 2
[[ "$RESUME_POSITION" =~ ^[0-9]+([.][0-9]+)?$ ]] ||
	die "RESUME_POSITION must be a non-negative number (got '$RESUME_POSITION')." 2
[[ "$ITEM_FILTER" =~ ^[0-9]+$ || -z "$ITEM_FILTER" ]] ||
	die "--item requires a numeric file id (got '$ITEM_FILTER')." 2
[[ "$SUBTITLE_ORDINAL" =~ ^[0-9]+$ || -z "$SUBTITLE_ORDINAL" ]] ||
	die "--subtitle-ordinal requires a non-negative integer (got '$SUBTITLE_ORDINAL')." 2

AUTH="Authorization: Bearer $API_KEY"

TMPDIR=$(mktemp -d)
RECORDS="$TMPDIR/records.jsonl"
SESSIONS="$TMPDIR/sessions"
: > "$RECORDS"
: > "$SESSIONS"

cleanup() {
	local rc=$?
	if [[ -f "$SESSIONS" ]]; then
		while IFS= read -r sid; do
			[[ -z "$sid" ]] && continue
			curl -sS --max-time 5 -X DELETE "$SERVER/api/v1/playback/$sid" \
				-H "$AUTH" -o /dev/null 2>/dev/null || true
		done < "$SESSIONS"
	fi
	rm -rf "$TMPDIR"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ── Helpers ───────────────────────────────────────────────────────────────────

# ms SECONDS -> milliseconds with one decimal. awk keeps this working on hosts
# whose python3 predates f-string float formatting.
ms() {
	awk -v v="${1:-0}" 'BEGIN{ if (v=="") v=0; printf "%.1f", v*1000 }' 2>/dev/null || echo "0"
}

# Absolute MiB/s over the whole request, including TTFB. Reported as measured.
mib_per_s() {
	awk -v b="${1:-0}" -v t="${2:-0}" 'BEGIN{ if (t>0 && b>0) printf "%.2f", (b/1048576.0)/(t/1000.0); else printf "0.00" }' 2>/dev/null || echo "0.00"
}

# RFC3339 UTC wall clock, millisecond precision, for log correlation.
ts_now() {
	date -u +%Y-%m-%dT%H:%M:%S.%3NZ
}

# Bracket one measured HTTP operation. rec() stamps the next record with the
# bracket and clears it, so every record carries started_at/ended_at.
PHASE_STARTED_AT=""
PHASE_ENDED_AT=""
phase_begin() {
	PHASE_STARTED_AT=$(ts_now)
	PHASE_ENDED_AT=""
}
phase_end() {
	PHASE_ENDED_AT=$(ts_now)
}

# Parse the total resource size the server reported, if any. Prefers the
# Content-Range total; falls back to Content-Length only on a 200 (a 206
# Content-Length describes the range, not the resource).
parse_total_size() {
	python3 - "${1:-}" "${2:-0}" <<'PY'
import re, sys
try:
    text = open(sys.argv[1], errors="replace").read()
except Exception:
    print("")
    raise SystemExit(0)
m = re.search(r"content-range:\s*bytes\s+\d+-\d+/(\d+)", text, re.I)
if m:
    print(m.group(1))
    raise SystemExit(0)
if str(sys.argv[2]) == "200":
    for line in text.splitlines():
        if line.lower().startswith("content-length:"):
            value = line.split(":", 1)[1].strip()
            if value.isdigit():
                print(value)
                raise SystemExit(0)
print("")
PY
}

# Parse the served byte range from a Content-Range header, or empty when the
# server did not report one (for example a 200 that ignored the Range header).
parse_served_range() {
	python3 - "${1:-}" <<'PY'
import re, sys
try:
    text = open(sys.argv[1], errors="replace").read()
except Exception:
    print("")
    raise SystemExit(0)
m = re.search(r"content-range:\s*bytes\s+(\d+)-(\d+)", text, re.I)
print(f"{m.group(1)}-{m.group(2)}" if m else "")
PY
}

# Score a bounded media read. A read that received the requested window is a
# success even when curl stopped at --max-filesize (exit 63); that truncation is
# recorded, not treated as failure. Echoes "valid|failure|truncated".
score_read() {
	local status="${1:-0}" rc="${2:-0}" bytes="${3:-0}" want="${4:-0}" body="$5" ctype="$6" failure
	if [[ "$status" -lt 200 || "$status" -ge 300 ]]; then
		echo "false|http_${status}|false"
		return
	fi
	if [[ "${bytes:-0}" -le 0 ]]; then
		echo "false|empty_body|false"
		return
	fi
	failure=$(validate_media "$body" "$ctype" "$bytes")
	if [[ "$failure" != "ok" ]]; then
		echo "false|$failure|false"
		return
	fi
	if [[ "${rc:-0}" -eq 0 ]]; then
		echo "true||false"
		return
	fi
	if [[ "${rc:-0}" -eq 63 ]]; then
		if [[ "${bytes:-0}" -ge "$want" ]]; then
			echo "true||true"
		else
			echo "false|truncated_before_window|true"
		fi
		return
	fi
	echo "false|curl_rc_${rc}|false"
}

# Strip credentials and the query string (signed tokens live there).
sanitize_url() {
	[[ -z "${1:-}" ]] && return 0
	python3 - "$1" <<'PY' || true
import sys
from urllib.parse import urlsplit
try:
    p = urlsplit(sys.argv[1])
except Exception:
    print("")
    raise SystemExit(0)
if not p.scheme:
    print(p.path)
else:
    print(f"{p.scheme}://{p.netloc}{p.path}")
PY
}

# Turn a plan-relative media path into a fetchable absolute URL. Plan stream and
# subtitle URLs are published relative to the API root.
resolve_media_url() {
	local u="${1:-}"
	[[ -z "$u" ]] && return 0
	case "$u" in
		http://*|https://*) printf '%s' "$u" ;;
		/api/*) printf '%s%s' "$SERVER" "$u" ;;
		/*) printf '%s/api/v1%s' "$SERVER" "$u" ;;
		*) printf '%s/%s' "$SERVER" "$u" ;;
	esac
}

# Read a dotted path out of a JSON file. Empty on missing file or field.
py_field() {
	python3 - "${1:-}" "${2:-}" <<'PY'
import json, sys
path, expr = sys.argv[1], sys.argv[2]
try:
    with open(path) as fh:
        cur = json.load(fh)
except Exception:
    print("")
    raise SystemExit(0)
for part in expr.split('.'):
    if not part:
        continue
    if isinstance(cur, dict):
        cur = cur.get(part)
    elif isinstance(cur, list):
        try:
            cur = cur[int(part)]
        except Exception:
            cur = None
    else:
        cur = None
    if cur is None:
        break
if cur is None:
    print("")
elif isinstance(cur, bool):
    print("true" if cur else "false")
else:
    print(cur)
PY
}

# Append one JSON record. Usage: rec <phase> key=value ...
# The bracketed wall-clock range (phase_begin/phase_end) is attached to the
# record and cleared, so a record without a bracket still gets a zero-width
# timestamp instead of a stale one.
rec() {
	local started="${PHASE_STARTED_AT:-}" ended="${PHASE_ENDED_AT:-}" now
	if [[ -z "$started" || -z "$ended" ]]; then
		now=$(ts_now)
		[[ -z "$started" ]] && started="$now"
		[[ -z "$ended" ]] && ended="$now"
	fi
	python3 "$TMPDIR/record.py" "$@" "started_at=$started" "ended_at=$ended" >> "$RECORDS"
	PHASE_STARTED_AT=""
	PHASE_ENDED_AT=""
}

# Fetch URL into body/hdr files. Prints "status|ttfb_s|total_s|bytes|ctype|rc".
http_get_tmo() {
	local tmo="$1" url="$2" body="$3" hdr="$4"
	shift 4
	local out rc=0
	out=$(curl -sS --max-time "$tmo" -D "$hdr" -o "$body" \
		-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -H "X-Device-Id: bench-e2e-$RUN_ID" \
		-w '%{http_code}|%{time_starttransfer}|%{time_total}|%{size_download}|%{content_type}' \
		"$@" "$url" 2>/dev/null) || rc=$?
	printf '%s|%s' "$out" "$rc"
}

http_get() {
	http_get_tmo "$CURL_TIMEOUT" "$@"
}

# HEAD a URL. Prints "status|ctype|rc" without a body, used to enumerate
# subtitle ordinals cheaply (a HEAD never spawns extraction).
http_head() {
	local url="$1" hdr="$2"
	shift 2
	local out rc=0
	out=$(curl -sS --max-time "$CURL_TIMEOUT" -I -D "$hdr" -o /dev/null \
		-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -H "X-Device-Id: bench-e2e-$RUN_ID" \
		-w '%{http_code}|%{content_type}' \
		"$@" "$url" 2>/dev/null) || rc=$?
	printf '%s|%s' "$out" "$rc"
}

# POST the protocol-v3 start request. Prints "status|ttfb_s|total_s|rc".
post_start() {
	local resp="$1" body="$2" attempt="$3"
	local out rc=0
	out=$(curl -sS --max-time "$CURL_TIMEOUT" -X POST "$SERVER/api/v1/playback/start" \
		-H "$AUTH" -H 'Content-Type: application/json' \
		-H "X-Profile-Id: $PROFILE_ID" -H "X-Device-Id: bench-e2e-$RUN_ID" \
		-o "$resp" \
		-w '%{http_code}|%{time_starttransfer}|%{time_total}' \
		--data-binary @"$body" 2>/dev/null) || rc=$?
	printf '%s|%s' "$out" "$rc"
}

# Validate a media body: non-empty, not a JSON error, not an HTML page.
validate_media() {
	local body="$1" ctype="$2" size="$3"
	[[ "${size:-0}" -le 0 ]] && { echo "empty_body"; return; }
	python3 - "$body" "$ctype" <<'PY'
import sys
body, ctype = sys.argv[1], (sys.argv[2] or "").lower()
with open(body, 'rb') as fh:
    head = fh.read(512).lstrip()
low = head[:64].lower()
if ctype.startswith('text/html') or low.startswith(b'<!doctype') or low.startswith(b'<html'):
    print('html_body'); raise SystemExit(0)
if ('json' in ctype or head[:1] in (b'{', b'[')) and head[:1] in (b'{', b'['):
    print('json_body'); raise SystemExit(0)
print('ok')
PY
}

# Classify a served subtitle body without trusting the requested extension: the
# extractor converts text tracks to WebVTT, so an ".ass" request proves ASS only
# when the body really is ASS. Echoes "format|valid|failure".
classify_subtitle() {
	local body="$1" ctype="$2"
	python3 - "$body" "$ctype" <<'PY'
import sys
path, ctype = sys.argv[1], (sys.argv[2] or "").lower()
try:
    data = open(path, 'rb').read(262144)
except Exception:
    print("unknown|false|unreadable")
    raise SystemExit(0)
if not data:
    print("unknown|false|empty")
    raise SystemExit(0)
head = data.lstrip()[:64].lower()
if 'json' in ctype or head[:1] in (b'{', b'['):
    print("unknown|false|json_error")
    raise SystemExit(0)
if ctype.startswith('text/html') or head.startswith(b'<!doctype') or head.startswith(b'<html'):
    print("unknown|false|html_body")
    raise SystemExit(0)
if data[:2] == b'PG':
    print("pgs|true|")
    raise SystemExit(0)
text = data.decode('utf-8', 'replace')
if '[script info]' in text.lower() or '[v4+' in text.lower() or '[v4 styles]' in text.lower():
    print("ass|true|")
    raise SystemExit(0)
if '-->' in text or text.lstrip().upper().startswith('WEBVTT'):
    print("vtt|true|")
    raise SystemExit(0)
print("unknown|false|unrecognized_payload")
PY
}

# Set or replace one query parameter, preserving every other parameter (the
# plan's stream URL already carries a signed token and a seek value).
set_query_param() {
	python3 - "$1" "$2" "$3" <<'PY'
import sys, urllib.parse as up
parts = up.urlsplit(sys.argv[1])
query = [(k, v) for k, v in up.parse_qsl(parts.query, keep_blank_values=True) if k != sys.argv[2]]
query.append((sys.argv[2], sys.argv[3]))
print(up.urlunsplit((parts.scheme, parts.netloc, parts.path, up.urlencode(query), parts.fragment)))
PY
}

# Deterministic random byte offsets beyond the first bounded range. Without a
# reported total, picks a varied offset in a plausible forward window rather
# than a fixed stride, so each seek request is distinct.
gen_offsets() {
	python3 - "$1" "$2" "$3" "$4" <<'PY'
import random, sys
total = int(sys.argv[1] or 0); rng = int(sys.argv[2]); n = int(sys.argv[3]); seed = int(sys.argv[4])
r = random.Random(seed)
out = []
for i in range(n):
    if total > rng:
        out.append(r.randint(rng, total - 1))
    else:
        out.append(r.randint(rng, rng * 32))
print("\n".join(str(x) for x in out))
PY
}


# Short content hash of a response body, used to tell a real seek response from
# a non-seek response that streamed the same bytes from the beginning.
body_sha() {
	sha256sum "${1:-/dev/null}" 2>/dev/null | cut -c1-16
}

# Deterministic random seek targets in seconds across the media timeline,
# keeping a margin from an excluded current position so a seek is a real jump.
gen_seek_seconds() {
	python3 - "$1" "$2" "$3" "${4:-}" <<'PY'
import random, sys
duration = float(sys.argv[1] or 0); n = int(sys.argv[2]); seed = int(sys.argv[3])
exclude = float(sys.argv[4]) if sys.argv[4] not in ("", None) else -1.0
r = random.Random(seed)
if duration > 0:
    lo = max(1.0, duration * 0.05)
    hi = max(lo + 1.0, duration * 0.95)
else:
    lo, hi = 10.0, 1800.0
margin = max(60.0, duration * 0.05) if duration > 0 else 60.0
for _ in range(n):
    target = r.uniform(lo, hi)
    for _attempt in range(30):
        if exclude < 0 or abs(target - exclude) >= margin:
            break
        target = r.uniform(lo, hi)
    print(f"{target:.1f}")
PY
}

# Deterministic random segment picks across the whole playlist as
# "segment_index<TAB>start_seconds<TAB>url", varied per iteration.
gen_hls_seek_picks() {
	python3 - "$1" "$2" "$3" <<'PY'
import random, sys
rows = [l.split("\t") for l in open(sys.argv[1]) if l.strip()]
rows = [r for r in rows if len(r) >= 4]
if not rows:
    raise SystemExit(0)
n = int(sys.argv[2]); seed = int(sys.argv[3])
r = random.Random(seed)
idxs = list(range(len(rows)))
r.shuffle(idxs)
for i in range(n):
    row = rows[idxs[i % len(idxs)]]
    print(f"{row[0]}\t{row[1]}\t{row[3]}")
PY
}

# Which server mechanism actually seeks for a delivery, or "" when unknown.
seek_mechanism() {
	case "$1" in
		server_remux_progressive) printf 'seek_param' ;;
		original_http|server_direct|direct) printf 'range' ;;
		server_remux_hls|server_transcode_hls) printf 'hls_segment' ;;
		*)
			case "$2" in
				hls) printf 'hls_segment' ;;
				http_progressive) printf 'range' ;;
				*) printf '' ;;
			esac
			;;
	esac
}

build_body() {
	local file_id=$1 attempt_id=$2 start=${3:-}
	local start_field=""
	[[ -n "$start" ]] && start_field="\"start_position\": $start,"
	cat <<EOF
{
  "protocol_version": 3,
  "file_id": $file_id,
  "profile_id": "$PROFILE_ID",
  "playback_attempt_id": "$attempt_id",
  "quality_preference": "automatic",
  $start_field
  "subtitle_fidelity_preference": "compatible",
  "client_capabilities": $CAPS,
  "client_playback_context": $CTX_PLAYABLE
}
EOF
}

# ── Embedded python helpers ───────────────────────────────────────────────────

cat > "$TMPDIR/record.py" <<'PY'
import json, sys

def conv(v):
    if v == "true":
        return True
    if v == "false":
        return False
    if v == "null":
        return None
    if v and v.lstrip("-").isdigit():
        try:
            return int(v)
        except ValueError:
            return v
    t = v.replace(".", "", 1).replace("-", "", 1).replace("e", "", 1).replace("E", "", 1)
    if v and t.isdigit():
        try:
            return float(v)
        except ValueError:
            return v
    return v

phase = sys.argv[1]
record = {"phase": phase}
for arg in sys.argv[2:]:
    key, _, value = arg.partition("=")
    if key:
        record[key] = conv(value)
print(json.dumps(record))
PY

cat > "$TMPDIR/parse_hls.py" <<'PY'
import sys
from urllib.parse import urljoin

path, base = sys.argv[1], sys.argv[2]
try:
    text = open(path, encoding="utf-8", errors="replace").read()
except Exception:
    raise SystemExit(0)

lines = [l.strip() for l in text.splitlines()]
if any(l.startswith("#EXT-X-STREAM-INF") for l in lines):
    for i, l in enumerate(lines):
        if l.startswith("#EXT-X-STREAM-INF"):
            for nxt in lines[i + 1:]:
                if nxt and not nxt.startswith("#"):
                    print("variant|" + urljoin(base, nxt))
                    raise SystemExit(0)
    raise SystemExit(0)

for l in lines:
    if l and not l.startswith("#"):
        print("segment|" + urljoin(base, l))
PY

# Resolve a media playlist into "index<TAB>start_seconds<TAB>duration<TAB>url",
# using EXT-X-MEDIA-SEQUENCE to anchor the first segment's start time.
cat > "$TMPDIR/hls_table.py" <<'PY'
import sys
from urllib.parse import urljoin

path, base = sys.argv[1], sys.argv[2]
try:
    lines = [l.strip() for l in open(path, encoding="utf-8", errors="replace").read().splitlines()]
except Exception:
    raise SystemExit(0)

media_sequence = 0
for l in lines:
    if l.startswith("#EXT-X-MEDIA-SEQUENCE:"):
        try:
            media_sequence = int(l.split(":", 1)[1])
        except ValueError:
            media_sequence = 0
        break

segments = []
duration = 0.0
for l in lines:
    if l.startswith("#EXTINF:"):
        try:
            duration = float(l.split(":", 1)[1].split(",")[0])
        except ValueError:
            duration = 0.0
    elif l and not l.startswith("#"):
        segments.append((l, duration))
        duration = 0.0

start = float(media_sequence)
for i, (uri, dur) in enumerate(segments):
    print(f"{i}\t{start:.3f}\t{dur:.3f}\t{urljoin(base, uri)}")
    start += dur
PY

# Sustained read with per-read timing. Fetches BENCH_URLS_FILE in order until
# BENCH_BYTES are read, records time to first byte and to the 256 KiB / 2 MiB
# milestones, and flags a stall when any single read attempt exceeds
# BENCH_STALL_MS. Credentials are passed through the environment, never argv.
cat > "$TMPDIR/throughput_read.py" <<'PY'
import json, os, socket, sys, time
import urllib.request
import urllib.error

urls = [l.strip() for l in open(os.environ["BENCH_URLS_FILE"]) if l.strip()]
target = int(os.environ["BENCH_BYTES"])
timeout = float(os.environ["BENCH_TIMEOUT"])
stall_ms = float(os.environ["BENCH_STALL_MS"])
auth = os.environ.get("BENCH_AUTH", "")
profile = os.environ.get("BENCH_PROFILE", "")
use_range = os.environ.get("BENCH_RANGE", "0") == "1"
chunk = 65536

start = time.monotonic()
first_byte_at = None
bytes_total = 0
status = 0
milestones = {262144: None, 2097152: None}
max_read_ms = 0.0
stalled = False
error = ""

try:
    for index, url in enumerate(urls):
        if bytes_total >= target:
            break
        headers = {}
        if auth:
            headers["Authorization"] = "Bearer " + auth
        if profile:
            headers["X-Profile-Id"] = profile
        if use_range and index == 0:
            headers["Range"] = f"bytes=0-{target - 1}"
        req = urllib.request.Request(url, headers=headers)
        resp = urllib.request.urlopen(req, timeout=timeout)
        status = resp.status
        while bytes_total < target:
            t0 = time.monotonic()
            data = resp.read(chunk)
            t1 = time.monotonic()
            if not data:
                break
            if first_byte_at is None:
                first_byte_at = t1
            else:
                elapsed = (t1 - t0) * 1000.0
                if elapsed > max_read_ms:
                    max_read_ms = elapsed
                if elapsed > stall_ms:
                    stalled = True
            bytes_total += len(data)
            for mark in milestones:
                if milestones[mark] is None and bytes_total >= mark:
                    milestones[mark] = (t1 - start) * 1000.0
        resp.close()
except urllib.error.HTTPError as exc:
    stalled = True
    status = exc.code
    error = f"http_{exc.code}"
except socket.timeout:
    stalled = True
    error = "timeout"
except Exception as exc:
    stalled = True
    error = type(exc).__name__

total_ms = (time.monotonic() - start) * 1000.0
ttfb_ms = None if first_byte_at is None else (first_byte_at - start) * 1000.0
mib_per_s = (bytes_total / 1048576.0) / (total_ms / 1000.0) if total_ms > 0 and bytes_total > 0 else 0.0
complete = bytes_total >= target and not error
print(json.dumps({
    "http_status": status,
    "bytes": bytes_total,
    "target_bytes": target,
    "ttfb_ms": ttfb_ms,
    "total_ms": total_ms,
    "mib_per_s": mib_per_s,
    "time_to_first_256kib_ms": milestones[262144],
    "time_to_first_2mib_ms": milestones[2097152],
    "stalled": stalled,
    "max_read_ms": max_read_ms,
    "complete": complete,
    "error": error,
}))
PY

cat > "$TMPDIR/aggregate.py" <<'PY'
import json, math, sys

records_path, meta_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
meta = json.load(open(meta_path))
records = [json.loads(l) for l in open(records_path) if l.strip()]

item_meta = {str(i["file_id"]): i for i in meta.get("items", [])}
item_ids = [str(i["file_id"]) for i in meta.get("items", [])]
phases = {}
for r in records:
    phases.setdefault(r["phase"], []).append(r)


def dist(values):
    nums = sorted(float(v) for v in values if isinstance(v, (int, float)) and not isinstance(v, bool))
    if not nums:
        return {"count": 0, "min": None, "p50": None, "p95": None, "max": None}

    def pct(p):
        k = max(0, min(len(nums) - 1, math.ceil(p / 100.0 * len(nums)) - 1))
        return nums[k]

    return {"count": len(nums), "min": nums[0], "p50": pct(50), "p95": pct(95), "max": nums[-1]}


def for_item(phase, item):
    return [r for r in phases.get(phase, []) if str(r.get("item")) == str(item)]


starts = phases.get("start", [])
playable = sum(1 for r in starts if r.get("playable") is True)
rejected = len(starts) - playable

first_all = phases.get("first_bytes", [])
first_attempted = [r for r in first_all if r.get("attempted") is True]
first_valid = [r for r in first_attempted if r.get("valid") is True]

seeks = phases.get("seek", [])
seek_attempted = [r for r in seeks if r.get("index") is not None]
seek_honoured = [r for r in seek_attempted if r.get("honoured") is True]
seek_mechanisms = {}
for r in seek_attempted:
    m = str(r.get("mechanism") or "unknown")
    seek_mechanisms[m] = seek_mechanisms.get(m, 0) + 1
seeks_by_item = {}
for r in seek_attempted:
    seeks_by_item.setdefault(str(r.get("item")), []).append(r)

throughput_all = [r for r in phases.get("throughput", []) if r.get("attempted") is True]
throughput_valid = [r for r in throughput_all if r.get("valid") is True]

subs = phases.get("subtitles", [])
sub_fetch = [r for r in subs if "warm" in r]
subs_cold = [r for r in sub_fetch if r.get("warm") is False]
subs_warm = [r for r in sub_fetch if r.get("warm") is True]
sub_plan = [r for r in subs if r.get("plan_only") is True]

resumes = phases.get("resume", [])
resume_attempted = [r for r in resumes if r.get("attempted") is True]
resume_honoured = [r for r in resume_attempted if r.get("resume_honoured") is True]

stops = phases.get("stop", [])
stop_attempted = [r for r in stops if r.get("attempted") is True]

summary = {
    "items": len(item_ids),
    "start": {
        "count": len(starts),
        "playable": playable,
        "rejected": rejected,
        "ttfb_ms": dist([r.get("ttfb_ms") for r in starts]),
        "total_ms": dist([r.get("total_ms") for r in starts]),
    },
    "first_bytes": {
        "attempted": len(first_attempted),
        "valid": len(first_valid),
        "ttfb_ms": dist([r.get("ttfb_ms") for r in first_valid]),
        "total_ms": dist([r.get("total_ms") for r in first_valid]),
        "mib_per_s": dist([r.get("mib_per_s") for r in first_valid]),
    },
    "throughput": {
        "count": len(throughput_all),
        "valid": len(throughput_valid),
        "stalled": sum(1 for r in throughput_all if r.get("stalled") is True),
        "bytes": dist([r.get("bytes") for r in throughput_valid]),
        "ttfb_ms": dist([r.get("ttfb_ms") for r in throughput_valid]),
        "total_ms": dist([r.get("total_ms") for r in throughput_valid]),
        "mib_per_s": dist([r.get("mib_per_s") for r in throughput_valid]),
        "time_to_first_256kib_ms": dist([r.get("time_to_first_256kib_ms") for r in throughput_valid]),
        "time_to_first_2mib_ms": dist([r.get("time_to_first_2mib_ms") for r in throughput_valid]),
    },
    "seek": {
        "count": len(seek_attempted),
        "skipped": len(seeks) - len(seek_attempted),
        "valid": len(seek_honoured),
        "honoured": len(seek_honoured),
        "mechanisms": seek_mechanisms,
        "ttfb_ms": dist([r.get("ttfb_ms") for r in seek_honoured]),
        "total_ms": dist([r.get("total_ms") for r in seek_honoured]),
        "by_item": {},
    },
    "resume": {
        "attempted": len(resume_attempted),
        "honoured": len(resume_honoured),
    },
    "subtitles": {
        "cold": dist([r.get("ttfb_ms") for r in subs_cold if r.get("valid") is True]),
        "warm": dist([r.get("ttfb_ms") for r in subs_warm if r.get("valid") is True]),
        "cold_count": len(subs_cold),
        "warm_count": len(subs_warm),
        "valid": sum(1 for r in subs if r.get("valid") is True),
        "tracks": max((int(r.get("track_count") or r.get("available_count") or 0)
                       for r in subs), default=0),
        "no_tracks": any(r.get("no_tracks") is True for r in subs),
        "plan_modes": sorted({str(r.get("plan_mode")) for r in subs if r.get("plan_mode")}),
        "inventory_count": max((int(r.get("inventory_count") or 0) for r in subs), default=0),
        "ordinal_source": (sub_plan[0].get("ordinal_source") if sub_plan else None),
    },
    "stop": {
        "count": len(stop_attempted),
        "total_ms": dist([r.get("total_ms") for r in stop_attempted]),
    },
}
for item, rows in seeks_by_item.items():
    honoured = [r for r in rows if r.get("honoured") is True]
    summary["seek"]["by_item"][item] = {
        "count": len(rows),
        "honoured": len(honoured),
        "mechanisms": sorted({str(r.get("mechanism") or "unknown") for r in rows}),
        "ttfb_ms": dist([r.get("ttfb_ms") for r in honoured]),
        "total_ms": dist([r.get("total_ms") for r in honoured]),
    }

report = {
    "meta": meta,
    "items": meta.get("items", []),
    "phases": phases,
    "summary": summary,
}

with open(out_path, "w") as fh:
    json.dump(report, fh, indent=2)
    fh.write("\n")


def fmt(v, suffix=""):
    return "n/a" if v is None else f"{v:.1f}{suffix}"


print("")
print("=== bench-playback-e2e — end-to-end playback measurement ===")
print(f"  server:  {meta.get('server', '')}")
print(f"  profile: {meta.get('profile_id', '')}")
print(f"  run:     {meta.get('run_id', '')}   git: {meta.get('git_revision') or 'unknown'}"
      + (" (dirty)" if meta.get("git_dirty") else ""))
print(f"  seed={meta.get('seed')}  sample_size={meta.get('sample_size')}  "
      f"seeks={meta.get('seeks')}  subtitle_repeats={meta.get('subtitle_repeats')}  "
      f"settle_ms={meta.get('settle_ms')}")
print("")
header = ("file_id", "type", "start", "ttfb", "first_ms", "fb", "thr_mib", "stall",
          "seek_mech", "seek_p50", "seek_p95", "resume", "sub_mode", "sub_cold", "sub_warm", "stop_ms")
widths = (10, 8, 22, 8, 9, 5, 8, 6, 12, 9, 9, 7, 9, 9, 9, 9)


def render(cols):
    return "  ".join(f"{str(c):<{w}}" for c, w in zip(cols, widths))


print("  " + render(header))
print("  " + "-" * (sum(widths) + 2 * (len(widths) - 1)))
for item in item_ids:
    im = item_meta.get(item, {})
    strand = for_item("start", item)
    fb = for_item("first_bytes", item)
    sk = for_item("seek", item)
    rs = for_item("resume", item)
    sub_rows = for_item("subtitles", item)
    sub_fetch = [r for r in sub_rows if "warm" in r]
    sc = [r for r in sub_fetch if r.get("warm") is False]
    sw = [r for r in sub_fetch if r.get("warm") is True]
    st = for_item("stop", item)
    outcome = strand[0].get("outcome") if strand else "?"
    sttfb = strand[0].get("ttfb_ms") if strand else None
    fb_rec = fb[0] if fb else {}
    fb_ttfb = fb_rec.get("ttfb_ms") if fb_rec.get("attempted") else None
    fb_valid = fb_rec.get("valid") if fb_rec.get("attempted") else None
    sk_rows = [r for r in sk if r.get("index") is not None]
    sk_hon = [r.get("ttfb_ms") for r in sk_rows if r.get("honoured") is True]
    sk_mech = ",".join(sorted({str(r.get("mechanism") or "?") for r in sk_rows})) or "n/a"
    thr_rows = [r for r in for_item("throughput", item) if r.get("attempted") is True]
    thr_valid = [r for r in thr_rows if r.get("valid") is True]
    thr_mib = fmt(dist([r.get("mib_per_s") for r in thr_valid]).get("p50"))
    stall = ("yes" if any(r.get("stalled") is True for r in thr_rows)
             else ("no" if thr_rows else "n/a"))
    resume_ok = rs[0].get("resume_honoured") if rs and rs[0].get("attempted") else None
    cold_ok = sum(1 for r in sc if r.get("valid") is True)
    warm_ok = sum(1 for r in sw if r.get("valid") is True)
    stop_ms = st[0].get("total_ms") if st and st[0].get("attempted") else None
    sub_modes = sorted({str(r.get("plan_mode")) for r in sub_rows if r.get("plan_mode")})
    sub_mode = ",".join(sub_modes) or "n/a"
    if any(r.get("no_tracks") for r in sub_rows):
        sub_cold = sub_warm = "no tracks"
    elif sub_fetch:
        sub_cold = f"{cold_ok}/{len(sc)}"
        sub_warm = f"{warm_ok}/{len(sw)}"
    elif sub_rows:
        sub_cold = sub_warm = "no fetch"
    else:
        sub_cold = sub_warm = "n/a"
    row = (
        str(item),
        str(im.get("type", ""))[:8],
        str(outcome),
        fmt(sttfb),
        fmt(fb_ttfb),
        ("ok" if fb_valid else "FAIL") if fb_rec.get("attempted") else "n/a",
        thr_mib,
        stall,
        sk_mech,
        fmt(dist(sk_hon).get("p50")),
        fmt(dist(sk_hon).get("p95")),
        ("yes" if resume_ok else "no") if resume_ok is not None else "n/a",
        sub_mode,
        sub_cold,
        sub_warm,
        fmt(stop_ms),
    )
    print("  " + render(row))

print("")
print("  ── Summary ──")

def dline(label, d, unit=""):
    if d.get("count"):
        print(f"  {label:<16} n={d['count']}  min={fmt(d['min'], unit)}  p50={fmt(d['p50'], unit)}  "
              f"p95={fmt(d['p95'], unit)}  max={fmt(d['max'], unit)}")
    else:
        print(f"  {label:<16} no data")

print(f"  items playable:  {playable}/{len(starts)}   rejected: {rejected}/{len(starts)}")
dline("start ttfb", summary["start"]["ttfb_ms"], "ms")
dline("start total", summary["start"]["total_ms"], "ms")
s = summary["first_bytes"]
print(f"  first bytes:     attempted={s['attempted']} valid={s['valid']}")
dline("  first ttfb", s["ttfb_ms"], "ms")
dline("  first total", s["total_ms"], "ms")
dline("  first mbps", s["mib_per_s"], " MiB/s")
t = summary["throughput"]
print(f"  throughput:      count={t['count']} valid={t['valid']} stalled={t['stalled']}")
dline("  thr ttfb", t["ttfb_ms"], "ms")
dline("  thr total", t["total_ms"], "ms")
dline("  thr mbps", t["mib_per_s"], " MiB/s")
dline("  first 256k", t["time_to_first_256kib_ms"], "ms")
dline("  first 2miB", t["time_to_first_2mib_ms"], "ms")
dline("seek ttfb", summary["seek"]["ttfb_ms"], "ms")
print(f"  seek:            honoured {summary['seek']['honoured']}/{summary['seek']['count']}"
      f"  mechanisms={summary['seek']['mechanisms']}"
      + (f"  skipped={summary['seek']['skipped']}" if summary["seek"]["skipped"] else ""))
print(f"  resume:          {summary['resume']['honoured']}/{summary['resume']['attempted']} honoured")
dline("sub cold ttfb", summary["subtitles"]["cold"], "ms")
dline("sub warm ttfb", summary["subtitles"]["warm"], "ms")
_sub = summary["subtitles"]
print(f"  subtitles:       plan_modes={_sub['plan_modes']} inventory={_sub['inventory_count']} "
      f"source={_sub['ordinal_source']} tracks={_sub['tracks']} "
      f"cold={_sub['cold_count']} warm={_sub['warm_count']} valid={_sub['valid']}"
      + ("  no tracks" if _sub["no_tracks"] else ""))
dline("stop", summary["stop"]["total_ms"], "ms")
print("")
PY

# ── Capability profiles ───────────────────────────────────────────────────────

CAPS='{
  "video_evidence": "declared",
  "audio_evidence": "declared",
  "codecs_video": ["h264", "hevc", "vp9", "av1"],
  "codecs_video_hardware": ["h264", "hevc"],
  "codecs_audio": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
  "containers": ["mp4", "mkv", "webm", "ts"],
  "max_resolution": "2160p",
  "hdr": true,
  "hdr_details": {"hdr10": true, "hlg": true, "dolby_vision": true}
}'

CTX_PLAYABLE='{
  "protocol_version": 3,
  "form_factor": "desktop",
  "app_version": "bench-e2e-1.0",
  "device": {"platform": "linux", "os_version": "6.1"},
  "deliveries": {
    "hls": {
      "enabled": true,
      "supported_on_device": true,
      "containers": ["hls", "mp4", "mkv", "ts"],
      "video_codecs": ["h264", "hevc", "vp9", "av1"],
      "audio_decode_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "audio_passthrough_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "subtitles": {"embedded_text": true, "sidecar_text": true}
    },
    "progressive": {
      "enabled": true,
      "supported_on_device": true,
      "containers": ["mp4", "mkv", "webm", "ts"],
      "video_codecs": ["h264", "hevc", "vp9", "av1"],
      "audio_decode_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "audio_passthrough_codecs": ["aac", "ac3", "eac3", "opus", "flac", "mp3"],
      "subtitles": {"embedded_text": true, "sidecar_text": true}
    }
  }
}'

# ── Discovery ─────────────────────────────────────────────────────────────────

fetch_catalog() {
	local lib="$1" typ="$2" out="$3"
	curl -sS --max-time "$CURL_TIMEOUT" -H "$AUTH" \
		"$SERVER/api/v1/catalog?library_id=$lib&limit=500&type=$typ" \
		-o "$out" >/dev/null 2>&1
}

fetch_catalog_all() {
	curl -sS --max-time "$CURL_TIMEOUT" -H "$AUTH" \
		"$SERVER/api/v1/catalog?limit=500" -o "$1" >/dev/null 2>&1
}

catalog_lines() {
	python3 - "$1" "$2" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    raise SystemExit(0)
for it in data.get("items") or []:
    cid = it.get("content_id", "")
    title = (it.get("title") or "").replace("\t", " ").replace("\n", " ")
    if cid:
        print(f"{cid}\t{title}\t{sys.argv[2]}")
PY
}

catalog_lines_all() {
	python3 - "$1" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    raise SystemExit(0)
for it in data.get("items") or []:
    cid = it.get("content_id", "")
    title = (it.get("title") or "").replace("\t", " ").replace("\n", " ")
    kind = it.get("type") or "item"
    if cid:
        print(f"{cid}\t{title}\t{kind}")
PY
}

discover_candidates() {
	local out lines combined="" found=0
	out="$TMPDIR/catalog-movies.json"
	if fetch_catalog "$MOVIE_LIBRARY_ID" movie "$out"; then
		lines=$(catalog_lines "$out" movie) || true
		[[ -n "$lines" ]] && { combined+="$lines"$'\n'; found=1; }
	fi
	out="$TMPDIR/catalog-episodes.json"
	if fetch_catalog "$SERIES_LIBRARY_ID" episode "$out"; then
		lines=$(catalog_lines "$out" episode) || true
		[[ -n "$lines" ]] && { combined+="$lines"$'\n'; found=1; }
	fi
	# A reconfigured deployment can leave the configured library ids empty or
	# disabled; fall back to the unfiltered catalog so discovery still works.
	if [[ "$found" -eq 0 ]]; then
		out="$TMPDIR/catalog-all.json"
		if fetch_catalog_all "$out"; then
			combined=$(catalog_lines_all "$out") || true
		fi
	fi
	printf '%s' "$combined"
}

sample_candidates() {
	python3 - "$1" "$SAMPLE_SIZE" "$SEED" <<'PY'
import random, sys
path, size, seed = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
lines = [l for l in open(path).read().splitlines() if l.strip()]
if size > 0 and len(lines) > size:
    r = random.Random(seed)
    idx = list(range(len(lines)))
    r.shuffle(idx)
    idx = sorted(idx[:size])
    lines = [lines[i] for i in idx]
print("\n".join(lines))
PY
}

resolve_file_id() {
	curl -sS --max-time "$CURL_TIMEOUT" -H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" \
		"$SERVER/api/v1/catalog/items/$1" 2>/dev/null > "$TMPDIR/item.json" || true
	python3 - "$TMPDIR/item.json" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    print("")
    raise SystemExit(0)
variants = data.get("playback_variants") or []
print(variants[0].get("default_file_id", "") if variants else "")
PY
}

# ── Phase implementations ─────────────────────────────────────────────────────

phase_first_bytes() {
	local fid="$1" plan_url="$2" proto="$3"
	local body="$TMPDIR/fb-${fid}.body" hdr="$TMPDIR/fb-${fid}.hdr"
	local full mode status ttfb total bytes ctype rc out
	local manifest_status="" manifest_ttfb="" total_size=""
	: > "$TMPDIR/segments-${fid}.txt"

	if [[ "$proto" == "hls" ]]; then
		mode="hls"
		local mbody="$TMPDIR/fb-${fid}.m3u8" mhdr="$TMPDIR/fb-${fid}.mhdr" mout
		full=$(resolve_media_url "$plan_url")
		mout=$(http_get "$full" "$mbody" "$mhdr")
		IFS='|' read -r manifest_status _ _ _ _ _ <<< "$mout"
		local parsed variant
		parsed=$(python3 "$TMPDIR/parse_hls.py" "$mbody" "$full")
		variant=$(printf '%s\n' "$parsed" | sed -n 's/^variant|//p' | sed -n '1p')
		if [[ -n "$variant" ]]; then
			full="$variant"
			mout=$(http_get "$full" "$mbody" "$mhdr")
			IFS='|' read -r manifest_status _ _ _ _ _ <<< "$mout"
			parsed=$(python3 "$TMPDIR/parse_hls.py" "$mbody" "$full")
		fi
		manifest_ttfb=$(printf '%s\n' "$mout" | cut -d'|' -f2)
		manifest_ttfb=$(ms "${manifest_ttfb:-0}")
		# Rich segment table (index/start/duration/url) drives HLS seeks.
		python3 "$TMPDIR/hls_table.py" "$mbody" "$full" > "$TMPDIR/hls-${fid}.tsv"
		local first_seg
		first_seg=$(sed -n '1p' "$TMPDIR/hls-${fid}.tsv" | cut -f4)
		if [[ -z "$first_seg" ]]; then
			rec first_bytes item="$fid" attempted=true mode="$mode" valid=false failure=no_segment \
				manifest_http_status="${manifest_status:-0}" manifest_ttfb_ms="${manifest_ttfb:-0}"
			return
		fi
		full="$first_seg"
	else
		mode="direct"
		full=$(resolve_media_url "$plan_url")
	fi

	phase_begin
	out=$(http_get "$full" "$body" "$hdr" --range "0-$((RANGE_BYTES - 1))" --max-filesize "$((RANGE_BYTES * 2))")
	phase_end
	IFS='|' read -r status ttfb total bytes ctype rc <<< "$out"
	total_size=$(parse_total_size "$hdr" "${status:-0}")
	local valid failure truncated verdict served_range="" range_ignored=false
	verdict=$(score_read "${status:-0}" "${rc:-0}" "${bytes:-0}" "$RANGE_BYTES" "$body" "${ctype:-}")
	IFS='|' read -r valid failure truncated <<< "$verdict"
	if [[ "${bytes:-0}" -gt 0 ]]; then
		served_range=$(parse_served_range "$hdr")
		if [[ -z "$served_range" ]]; then
			served_range="0-$((bytes - 1))"
			range_ignored=true
		fi
	fi
	local mib
	mib=$(mib_per_s "${bytes:-0}" "$(ms "${total:-0}")")
	rec first_bytes item="$fid" attempted=true mode="$mode" request="$(sanitize_url "$full")" \
		http_status="${status:-0}" ttfb_ms="$(ms "${ttfb:-0}")" total_ms="$(ms "${total:-0}")" \
		bytes="${bytes:-0}" mib_per_s="$mib" content_type="${ctype:-}" valid="$valid" \
		failure="$failure" total_size_bytes="${total_size:-0}" \
		truncated_by_max_filesize="$truncated" range_requested="bytes=0-$((RANGE_BYTES - 1))" \
		served_range="$served_range" range_ignored="$range_ignored" requested_bytes="$RANGE_BYTES" \
		manifest_http_status="${manifest_status:-0}" manifest_ttfb_ms="${manifest_ttfb:-0}"
}

# ── Sustained throughput ───────────────────────────────────────────────────────

# Write the URL list for the throughput read and echo the mode: a single stream
# URL for progressive/direct, or the HLS segment list for HLS deliveries.
throughput_urls() {
	local fid="$1" delivery="$2" proto="$3" plan_url="$4"
	local out="$TMPDIR/thr-urls-${fid}.txt"
	: > "$out"
	case "$(seek_mechanism "$delivery" "$proto")" in
		range)
			resolve_media_url "$plan_url" > "$out"
			printf 'range'
			;;
		seek_param)
			resolve_media_url "$plan_url" > "$out"
			printf 'stream'
			;;
		hls_segment)
			if [[ -s "$TMPDIR/hls-${fid}.tsv" ]]; then
				cut -f4 "$TMPDIR/hls-${fid}.tsv" > "$out"
				printf 'segments'
			fi
			;;
		*) printf '' ;;
	esac
}

phase_throughput() {
	local fid="$1" delivery="$2" proto="$3" plan_url="$4"
	local mode
	mode=$(throughput_urls "$fid" "$delivery" "$proto" "$plan_url")
	if [[ -z "$mode" ]]; then
		rec throughput item="$fid" attempted=false valid=false \
			reason="unsupported_delivery_${delivery:-unknown}"
		return
	fi
	local range_flag=0
	[[ "$mode" == "range" ]] && range_flag=1
	phase_begin
	local json
	json=$(BENCH_URLS_FILE="$TMPDIR/thr-urls-${fid}.txt" \
		BENCH_BYTES="$THROUGHPUT_BYTES" BENCH_TIMEOUT="$CURL_TIMEOUT" \
		BENCH_STALL_MS="$STALL_MS" BENCH_AUTH="$API_KEY" BENCH_PROFILE="$PROFILE_ID" \
		BENCH_RANGE="$range_flag" python3 "$TMPDIR/throughput_read.py" 2>/dev/null) || json="{}"
	phase_end
	local out
	out=$(python3 - "$json" <<'PY'
import json, sys
try:
    d = json.loads(sys.argv[1])
except Exception:
    d = {}
keys = ("http_status", "bytes", "target_bytes", "ttfb_ms", "total_ms", "mib_per_s",
        "time_to_first_256kib_ms", "time_to_first_2mib_ms", "stalled", "max_read_ms",
        "complete", "error")
print("|".join("" if d.get(k) is None else str(d.get(k, "")) for k in keys))
PY
)
	IFS='|' read -r http_status bytes target ttfb total mib first256 first2 stalled maxread complete error <<< "$out"
	[[ "$stalled" == "True" ]] && stalled=true
	[[ "$stalled" == "False" ]] && stalled=false
	[[ "$complete" == "True" ]] && complete=true
	[[ "$complete" == "False" ]] && complete=false
	local valid=false
	[[ "$complete" == "true" ]] && valid=true
	rec throughput item="$fid" attempted=true mode="$mode" mechanism="$(seek_mechanism "$delivery" "$proto")" \
		http_status="${http_status:-0}" bytes="${bytes:-0}" target_bytes="${target:-$THROUGHPUT_BYTES}" \
		ttfb_ms="${ttfb:-}" total_ms="${total:-}" mib_per_s="${mib:-}" \
		time_to_first_256kib_ms="${first256:-}" time_to_first_2mib_ms="${first2:-}" \
		stalled="${stalled:-false}" max_read_ms="${maxread:-}" complete="${complete:-false}" \
		valid="$valid" failure="${error:-}"
}

# ── Subtitle discovery and delivery ────────────────────────────────────────────

# Start a throwaway session and echo its id. The session is registered for
# cleanup. A completed subtitle extraction can end its session, so each fetch
# gets its own.
start_probe_session() {
	local fid="$1" tag="$2"
	local attempt="e2e-${RUN_ID}-${fid}-${tag}"
	local body="$TMPDIR/${tag}-${fid}.req.json"
	local resp="$TMPDIR/${tag}-${fid}.json"
	build_body "$fid" "$attempt" > "$body"
	post_start "$resp" "$body" "$attempt" >/dev/null
	local sid
	sid=$(py_field "$resp" session_id)
	[[ -n "$sid" ]] && printf '%s\n' "$sid" >> "$SESSIONS"
	printf '%s' "$sid"
}

# HEAD-enumerate subtitle ordinals on a session. Prints "ordinal<TAB>family"
# for each present ordinal; family is text or bitmap. Cheap: no extraction.
subtitle_ordinals() {
	local sid="$1" fid="$2" n=0
	while [[ "$n" -lt "$SUBTITLE_ORDINALS" ]]; do
		local vtt sup vstatus sstatus sctype
		vtt=$(http_head "$SERVER/api/v1/stream/$sid/subtitles/$n.vtt?file_id=$fid" "$TMPDIR/h-${sid}-${n}.hdr")
		IFS='|' read -r vstatus _ _ <<< "$vtt"
		sup=$(http_head "$SERVER/api/v1/stream/$sid/subtitles/$n.sup?file_id=$fid" "$TMPDIR/h-${sid}-${n}sup.hdr")
		IFS='|' read -r sstatus sctype _ <<< "$sup"
		local present=0 family="text"
		if [[ "$vstatus" -ge 200 && "$vstatus" -lt 300 ]]; then
			present=1
		fi
		if [[ "$sstatus" -ge 200 && "$sstatus" -lt 300 ]]; then
			present=1
			case "$sctype" in
				*text/*|*json*) family="text" ;;
				*) family="bitmap" ;;
			esac
		fi
		[[ "$present" -eq 1 ]] && printf '%s\t%s\n' "$n" "$family"
		n=$((n + 1))
	done
}

# Candidate subtitle representations from the plan inventory, as
# "ordinal<TAB>ext<TAB>track_id". Empty when the plan published no sidecar URLs
# (which does not mean the file has no tracks).
subtitle_candidates_from_plan() {
	python3 - "$1" <<'PY'
import json, sys, urllib.parse
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    raise SystemExit(0)
plan = data.get("playback_plan") or {}
inventory = ((plan.get("subtitle") or {}).get("inventory")) or []
for item in inventory:
    url = item.get("url") or ""
    if item.get("delivery") != "sidecar" or not url:
        continue
    path = urllib.parse.urlparse(url).path
    ext = path.rsplit(".", 1)[-1].lower() if "." in path else "vtt"
    print(f"{item.get('combined_index', '')}\t{ext}\t{item.get('track_id', '')}")
PY
}

# Fetch one subtitle representation on a fresh session, classify the payload,
# and record it. warm=false is the cold extraction, warm=true a repeat.
subtitle_fetch() {
	local fid="$1" ordinal="$2" ext="$3" warm="$4" attempt_n="$5" track_id="${6:-}"
	local body="$TMPDIR/sf-${fid}-${ordinal}-${ext}.body"
	local hdr="$TMPDIR/sf-${fid}-${ordinal}-${ext}.hdr"
	local sid
	sid=$(start_probe_session "$fid" "sub${ordinal}${warm}${attempt_n}")
	if [[ -z "$sid" ]]; then
		rec subtitles item="$fid" ordinal="$ordinal" track_id="$track_id" warm="$warm" \
			attempt="$attempt_n" valid=false failure=session_unavailable
		return
	fi
	local url="$SERVER/api/v1/stream/$sid/subtitles/$ordinal.$ext?file_id=$fid"
	local out status ttfb total bytes ctype rc
	phase_begin
	out=$(http_get_tmo "$SUBTITLE_TIMEOUT" "$url" "$body" "$hdr")
	phase_end
	IFS='|' read -r status ttfb total bytes ctype rc <<< "$out"
	local cls fmt valid failure timed_out=false
	cls=$(classify_subtitle "$body" "${ctype:-}")
	IFS='|' read -r fmt valid failure <<< "$cls"
	local failure_reason="$failure"
	if [[ "$rc" != "0" ]]; then
		if [[ "$valid" == "true" && "$rc" == "28" ]]; then
			timed_out=true
		else
			failure_reason="curl_rc_${rc}"
		fi
	fi
	rec subtitles item="$fid" ordinal="$ordinal" track_id="$track_id" warm="$warm" \
		attempt="$attempt_n" request="$(sanitize_url "$url")" http_status="${status:-0}" \
		ttfb_ms="$(ms "${ttfb:-0}")" total_ms="$(ms "${total:-0}")" bytes="${bytes:-0}" \
		content_type="${ctype:-}" format="$fmt" valid="$valid" failure="$failure_reason" \
		timed_out="$timed_out"
	curl -sS --max-time 10 -X DELETE "$SERVER/api/v1/playback/$sid" \
		-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -o /dev/null 2>/dev/null || true
}

# ── Seek mechanisms ───────────────────────────────────────────────────────────

# Progressive/remux: the stream handler seeks when the URL carries ?seek=<sec>.
# Honour is verified by comparing the response body to a non-seek baseline: if
# the server ignored the parameter it streams the same bytes from the beginning.
seek_via_param() {
	local fid="$1" plan_url="$2" duration="$3"
	local full
	full=$(resolve_media_url "$plan_url")
	local body="$TMPDIR/sp-${fid}.body" hdr="$TMPDIR/sp-${fid}.hdr"
	local out status ttfb total bytes ctype rc baseline_sha sha
	local verdict valid failure truncated honoured reason
	phase_begin
	out=$(http_get "$full" "$body" "$hdr" --range "0-$((RANGE_BYTES - 1))" --max-filesize "$((RANGE_BYTES * 2))")
	phase_end
	baseline_sha=""
	[[ -s "$body" ]] && baseline_sha=$(body_sha "$body")

	local secs s i=0 baseline_pos=""
	baseline_pos=$(python3 - "$full" <<'PY'
import sys, urllib.parse as up
q = dict(up.parse_qsl(up.urlsplit(sys.argv[1]).query))
print(q.get("seek", ""))
PY
)
	secs=$(gen_seek_seconds "$duration" "$SEEKS" "$((SEED + fid))" "$baseline_pos")
	while IFS= read -r s; do
		[[ -z "$s" ]] && continue
		local seek_url
		seek_url=$(set_query_param "$full" seek "$s")
		phase_begin
		out=$(http_get "$seek_url" "$body" "$hdr" --max-filesize "$((RANGE_BYTES * 2))")
		phase_end
		IFS='|' read -r status ttfb total bytes ctype rc <<< "$out"
		verdict=$(score_read "${status:-0}" "${rc:-0}" "${bytes:-0}" "$RANGE_BYTES" "$body" "${ctype:-}")
		IFS='|' read -r valid failure truncated <<< "$verdict"
		sha=$(body_sha "$body")
		if [[ "$valid" != "true" ]]; then
			honoured=false; reason="${failure:-invalid_read}"
		elif [[ -z "$baseline_sha" ]]; then
			honoured=false; reason="baseline_unavailable"
		elif [[ "$sha" != "$baseline_sha" ]]; then
			honoured=true; reason=""
		else
			honoured=false; reason="same_bytes_as_non_seek"
		fi
		rec seek item="$fid" index="$i" attempted=true mechanism=seek_param honoured="$honoured" \
			seek_seconds="$s" request="$(sanitize_url "$seek_url")" http_status="${status:-0}" \
			ttfb_ms="$(ms "${ttfb:-0}")" total_ms="$(ms "${total:-0}")" bytes="${bytes:-0}" \
			content_type="${ctype:-}" valid="$honoured" failure="$reason" \
			seek_sha256="$sha" baseline_sha256="$baseline_sha"
		i=$((i + 1))
	done <<< "$secs"
}

# Direct/original HTTP: the server honours byte ranges, so a successful 206
# whose served start is the requested offset is the seek.
seek_via_range() {
	local fid="$1" plan_url="$2" total_size="$3"
	local full
	full=$(resolve_media_url "$plan_url")
	local body="$TMPDIR/sr-${fid}.body" hdr="$TMPDIR/sr-${fid}.hdr"
	local offsets out status ttfb total bytes ctype rc off i=0
	offsets=$(gen_offsets "$total_size" "$RANGE_BYTES" "$SEEKS" "$((SEED + fid))")
	while IFS= read -r off; do
		[[ -z "$off" ]] && continue
		phase_begin
		out=$(http_get "$full" "$body" "$hdr" --range "$off-$((off + RANGE_BYTES - 1))" --max-filesize "$((RANGE_BYTES * 2))")
		phase_end
		IFS='|' read -r status ttfb total bytes ctype rc <<< "$out"
		local verdict valid failure truncated served_range="" range_ignored=false honoured=false reason=""
		verdict=$(score_read "${status:-0}" "${rc:-0}" "${bytes:-0}" "$RANGE_BYTES" "$body" "${ctype:-}")
		IFS='|' read -r valid failure truncated <<< "$verdict"
		if [[ "${bytes:-0}" -gt 0 ]]; then
			served_range=$(parse_served_range "$hdr")
			if [[ -z "$served_range" ]]; then
				served_range="0-$((bytes - 1))"
				range_ignored=true
			fi
		fi
		if [[ "$valid" == "true" && "$range_ignored" == "false" && "${served_range%%-*}" == "$off" ]]; then
			honoured=true
		elif [[ "$range_ignored" == "true" ]]; then
			reason="range_ignored"
		else
			reason="${failure:-served_range_mismatch}"
		fi
		rec seek item="$fid" index="$i" attempted=true mechanism=range honoured="$honoured" \
			offset="$off" request="$(sanitize_url "$full")" http_status="${status:-0}" \
			ttfb_ms="$(ms "${ttfb:-0}")" total_ms="$(ms "${total:-0}")" bytes="${bytes:-0}" \
			valid="$honoured" failure="$reason" truncated_by_max_filesize="$truncated" \
			range_requested="bytes=$off-$((off + RANGE_BYTES - 1))" served_range="$served_range" \
			range_ignored="$range_ignored" requested_bytes="$RANGE_BYTES"
		i=$((i + 1))
	done <<< "$offsets"
}

# HLS: a seek is a request for a segment further down the playlist.
seek_via_hls() {
	local fid="$1"
	local table="$TMPDIR/hls-${fid}.tsv"
	local body="$TMPDIR/sh-${fid}.body" hdr="$TMPDIR/sh-${fid}.hdr"
	if [[ ! -s "$table" ]]; then
		rec seek item="$fid" attempted=false reason=no_segments mechanism=hls_segment valid=false
		return
	fi
	local picks
	picks=$(gen_hls_seek_picks "$table" "$SEEKS" "$((SEED + fid))")
	if [[ -z "$picks" ]]; then
		rec seek item="$fid" attempted=false reason=no_segments mechanism=hls_segment valid=false
		return
	fi
	local out status ttfb total bytes ctype rc seg_idx seg_start url i=0
	while IFS=$'\t' read -r seg_idx seg_start url; do
		[[ -z "$url" ]] && continue
		phase_begin
		out=$(http_get "$url" "$body" "$hdr")
		phase_end
		IFS='|' read -r status ttfb total bytes ctype rc <<< "$out"
		local verdict valid failure truncated
		verdict=$(score_read "${status:-0}" "${rc:-0}" "${bytes:-0}" 1 "$body" "${ctype:-}")
		IFS='|' read -r valid failure truncated <<< "$verdict"
		rec seek item="$fid" index="$i" attempted=true mechanism=hls_segment honoured="$valid" \
			segment_index="$seg_idx" segment_start_seconds="$seg_start" request="$(sanitize_url "$url")" \
			http_status="${status:-0}" ttfb_ms="$(ms "${ttfb:-0}")" total_ms="$(ms "${total:-0}")" \
			bytes="${bytes:-0}" content_type="${ctype:-}" valid="$valid" failure="${failure:-}"
		i=$((i + 1))
	done <<< "$picks"
}

phase_seek() {
	local fid="$1" delivery="$2" proto="$3" plan_url="$4" total_size="$5" duration="$6"
	local mech
	mech=$(seek_mechanism "$delivery" "$proto")
	case "$mech" in
		seek_param) seek_via_param "$fid" "$plan_url" "$duration" ;;
		range) seek_via_range "$fid" "$plan_url" "$total_size" ;;
		hls_segment) seek_via_hls "$fid" ;;
		*) rec seek item="$fid" attempted=false mechanism=unsupported \
			reason="unsupported_delivery_${delivery:-unknown}" valid=false ;;
	esac
}

# Subtitle delivery. The plan's inventory is only populated when a subtitle is
# selected, so an empty inventory is not proof the file has no tracks. Tracks
# are enumerated from the plan when present, otherwise by HEAD-probing combined
# ordinals on a throwaway session, then each available track is fetched through
# the stream subtitle endpoint. A completed extraction can end its session, so
# cold and each warm fetch use their own session; the server caches the
# extracted artifact, which is what makes the warm fetch fast.
phase_subtitles() {
	local fid="$1" resp="$2"
	local plan_mode inv_count
	plan_mode=$(py_field "$resp" playback_plan.subtitle.mode)
	inv_count=$(subtitle_candidates_from_plan "$resp" | grep -c . || true)
	[[ -z "$inv_count" ]] && inv_count=0

	local cands="$TMPDIR/subs-${fid}.tsv"
	subtitle_candidates_from_plan "$resp" > "$cands"
	local source="plan"
	if [[ ! -s "$cands" ]]; then
		source="probe"
		local probe_sid ords
		probe_sid=$(start_probe_session "$fid" "subprobe")
		if [[ -z "$probe_sid" ]]; then
			rec subtitles item="$fid" plan_mode="${plan_mode:-unknown}" inventory_count="$inv_count" \
				ordinal_source="$source" no_tracks=true valid=false failure=session_unavailable
			return
		fi
		ords=$(subtitle_ordinals "$probe_sid" "$fid")
		while IFS=$'\t' read -r ordinal family; do
			[[ -z "$ordinal" ]] && continue
			local ext="ass"
			[[ "$family" == "bitmap" ]] && ext="sup"
			printf '%s\t%s\t\n' "$ordinal" "$ext" >> "$cands"
		done <<< "$ords"
		curl -sS --max-time 10 -X DELETE "$SERVER/api/v1/playback/$probe_sid" \
			-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -o /dev/null 2>/dev/null || true
	fi

	# Honor --subtitle-ordinal by narrowing to one combined ordinal.
	if [[ -n "$SUBTITLE_ORDINAL" && -s "$cands" ]]; then
		awk -F'\t' -v want="$SUBTITLE_ORDINAL" '$1 == want' "$cands" > "$cands.filtered" || true
		mv "$cands.filtered" "$cands"
	fi

	local track_count
	track_count=$(grep -c . "$cands" 2>/dev/null || true)
	[[ -z "$track_count" ]] && track_count=0
	rec subtitles item="$fid" plan_only=true plan_mode="${plan_mode:-unknown}" \
		inventory_count="$inv_count" ordinal_source="$source" track_count="$track_count" \
		available_count="$track_count" valid=false

	if [[ "$track_count" -eq 0 ]]; then
		if [[ -n "$SUBTITLE_ORDINAL" ]]; then
			rec subtitles item="$fid" plan_mode="${plan_mode:-unknown}" inventory_count="$inv_count" \
				ordinal_source="$source" requested_ordinal="$SUBTITLE_ORDINAL" reason=no_such_ordinal valid=false
		else
			rec subtitles item="$fid" plan_mode="${plan_mode:-unknown}" inventory_count="$inv_count" \
				ordinal_source="$source" no_tracks=true valid=false
		fi
		return
	fi

	local ordinal ext track_id i
	while IFS=$'\t' read -r ordinal ext track_id; do
		[[ -z "$ordinal" ]] && continue
		[[ -z "$ext" ]] && ext="ass"
		subtitle_fetch "$fid" "$ordinal" "$ext" false 0 "$track_id"
		for i in $(seq 1 "$SUBTITLE_REPEATS"); do
			[[ "$SETTLE_MS" -gt 0 ]] && sleep "$(awk -v m="$SETTLE_MS" 'BEGIN{printf "%.3f", m/1000}')"
			subtitle_fetch "$fid" "$ordinal" "$ext" true "$i" "$track_id"
		done
	done < "$cands"
}

# Cold-fetch one representation to learn its real format, without recording a
# phase record. Echoes the classified format (ass, pgs, vtt, unknown).
subtitle_probe_format() {
	local fid="$1" ordinal="$2" ext="$3"
	local sid
	sid=$(start_probe_session "$fid" "ft${ordinal}${ext}")
	if [[ -z "$sid" ]]; then
		printf 'unknown'
		return
	fi
	local body="$TMPDIR/ft-${fid}-${ordinal}.${ext}.body"
	local hdr="$TMPDIR/ft-${fid}-${ordinal}.${ext}.hdr"
	local out status ttfb total bytes ctype rc
	out=$(http_get_tmo "$SUBTITLE_TIMEOUT" "$SERVER/api/v1/stream/$sid/subtitles/$ordinal.$ext?file_id=$fid" "$body" "$hdr")
	IFS='|' read -r status ttfb total bytes ctype rc <<< "$out"
	local cls fmt
	cls=$(classify_subtitle "$body" "${ctype:-}")
	fmt="${cls%%|*}"
	curl -sS --max-time 10 -X DELETE "$SERVER/api/v1/playback/$sid" \
		-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -o /dev/null 2>/dev/null || true
	printf '%s' "$fmt"
}

# --find-subtitles: scan up to FIND_SUBTITLES_LIMIT items and report the plan's
# subtitle mode, whether it published an inventory, the combined ordinals that
# resolve, and the real format of one text and one bitmap candidate. HEAD
# probes are cheap; format classification needs one cold fetch per family.
run_find_subtitles() {
	local limit="$FIND_SUBTITLES_LIMIT"
	local results="$TMPDIR/find-subs.jsonl"
	: > "$results"
	local list
	if [[ -n "$ITEM_FILTER" ]]; then
		list="$ITEM_FILTER	item	item"
	else
		list=$(discover_candidates) || true
	fi
	if [[ -z "$list" ]]; then
		die "no candidates discovered from libraries $MOVIE_LIBRARY_ID/$SERIES_LIBRARY_ID; pass --item <file_id>" 3
	fi

	local scanned=0 with_tracks=0
	while IFS=$'\t' read -r cid title typ; do
		[[ -z "$cid" ]] && continue
		[[ "$scanned" -ge "$limit" ]] && break
		local fid="$cid"
		if [[ -z "$ITEM_FILTER" ]]; then
			fid=$(resolve_file_id "$cid") || true
			[[ -z "$fid" ]] && continue
		fi
		scanned=$((scanned + 1))

		local sid mode inv_count
		sid=$(start_probe_session "$fid" "ftprobe")
		if [[ -z "$sid" ]]; then
			printf '  file_id=%s  <unplayable>  %s\n' "$fid" "$title"
			continue
		fi
		mode=$(py_field "$TMPDIR/ftprobe-${fid}.json" playback_plan.subtitle.mode)
		inv_count=$(subtitle_candidates_from_plan "$TMPDIR/ftprobe-${fid}.json" | grep -c . || true)
		[[ -z "$inv_count" ]] && inv_count=0

		local ords candidates="$TMPDIR/ft-cands-${fid}.tsv"
		: > "$candidates"
		subtitle_candidates_from_plan "$TMPDIR/ftprobe-${fid}.json" |
			awk -F'\t' '{ family=($2=="sup")?"bitmap":"text"; print $1"\t"$2"\t"family"\t"$3 }' > "$candidates"
		if [[ ! -s "$candidates" ]]; then
			ords=$(subtitle_ordinals "$sid" "$fid")
			while IFS=$'\t' read -r ordinal family; do
				[[ -z "$ordinal" ]] && continue
				local ext="ass"
				[[ "$family" == "bitmap" ]] && ext="sup"
				printf '%s\t%s\t%s\t\n' "$ordinal" "$ext" "$family" >> "$candidates"
			done <<< "$ords"
		fi
		curl -sS --max-time 10 -X DELETE "$SERVER/api/v1/playback/$sid" \
			-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -o /dev/null 2>/dev/null || true

		local ordinal_list="" text_ord="" text_ext="" bitmap_ord="" bitmap_ext="" family
		while IFS=$'\t' read -r ordinal ext family _; do
			[[ -z "$ordinal" ]] && continue
			ordinal_list="${ordinal_list}${ordinal}:${family} "
			if [[ "$family" == "bitmap" ]]; then
				[[ -z "$bitmap_ext" ]] && { bitmap_ord="$ordinal"; bitmap_ext="$ext"; }
			else
				[[ -z "$text_ext" ]] && { text_ord="$ordinal"; text_ext="ass"; }
			fi
		done < "$candidates"

		local formats="" fmt
		if [[ -n "$text_ext" ]]; then
			fmt=$(subtitle_probe_format "$fid" "$text_ord" "$text_ext")
			formats="${formats}ordinal ${text_ord}=${fmt} "
		fi
		if [[ -n "$bitmap_ext" ]]; then
			fmt=$(subtitle_probe_format "$fid" "$bitmap_ord" "$bitmap_ext")
			formats="${formats}ordinal ${bitmap_ord}=${fmt} "
		fi
		local track_count
		track_count=$(grep -c . "$candidates" 2>/dev/null || true)
		[[ -z "$track_count" ]] && track_count=0
		[[ "$track_count" -gt 0 ]] && with_tracks=$((with_tracks + 1))

		META_FID="$fid" META_TITLE="$title" META_TYPE="$typ" META_MODE="${mode:-unknown}" \
		META_INV="$inv_count" META_ORDS="$ordinal_list" META_FORMATS="$formats" \
		META_TRACKS="$track_count" python3 - "$results" <<'PY'
import json, os, sys
rec = {
    "file_id": int(os.environ["META_FID"]) if os.environ["META_FID"].isdigit() else os.environ["META_FID"],
    "title": os.environ["META_TITLE"],
    "type": os.environ["META_TYPE"],
    "plan_subtitle_mode": os.environ["META_MODE"],
    "plan_inventory_count": int(os.environ["META_INV"]),
    "track_count": int(os.environ["META_TRACKS"]),
    "ordinals": os.environ["META_ORDS"].split(),
    "formats": os.environ["META_FORMATS"].strip(),
}
with open(sys.argv[1], "a") as fh:
    fh.write(json.dumps(rec) + "\n")
PY
		printf '  file_id=%-9s mode=%-6s inventory=%s tracks=%s ordinals=%s formats=%s  %s\n' \
			"$fid" "${mode:-?}" "$inv_count" "$track_count" "${ordinal_list:-none}" "${formats:-none}" "$title"
	done <<< "$list"

	echo ""
	if [[ "$with_tracks" -eq 0 ]]; then
		echo "  scanned $scanned item(s); none expose subtitle tracks."
	else
		echo "  scanned $scanned item(s); $with_tracks expose subtitle tracks."
	fi

	if [[ -n "$JSON_OUT" ]]; then
		META_SERVER="$(sanitize_url "$SERVER")" META_PROFILE="$PROFILE_ID" \
		META_RUN_ID="$RUN_ID" META_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		META_SCANNED="$scanned" META_WITH="$with_tracks" META_RESULTS="$results" \
		python3 - "$JSON_OUT" <<'PY'
import json, os, sys
items = [json.loads(l) for l in open(os.environ["META_RESULTS"]) if l.strip()]
report = {
    "meta": {
        "tool": "bench-playback-e2e",
        "mode": "find_subtitles",
        "server": os.environ["META_SERVER"],
        "profile_id": os.environ["META_PROFILE"],
        "run_id": os.environ["META_RUN_ID"],
        "timestamp": os.environ["META_TIMESTAMP"],
        "scanned": int(os.environ["META_SCANNED"]),
        "with_tracks": int(os.environ["META_WITH"]),
    },
    "items": items,
}
json.dump(report, open(sys.argv[1], "w"), indent=2)
PY
		echo "JSON report written to $JSON_OUT"
	fi
	exit 0
}

# Stop the primary session. Measured as the stop phase.
phase_stop() {
	local fid="$1" sid="$2"
	if [[ -z "$sid" ]]; then
		rec stop item="$fid" attempted=false reason=no_session
		return
	fi
	local result status total
	phase_begin
	result=$(curl -sS --max-time "$CURL_TIMEOUT" -X DELETE "$SERVER/api/v1/playback/$sid" \
		-H "$AUTH" -H "X-Profile-Id: $PROFILE_ID" -o /dev/null \
		-w '%{http_code}|%{time_total}' 2>/dev/null || echo "0|0")
	phase_end
	IFS='|' read -r status total <<< "$result"
	rec stop item="$fid" attempted=true http_status="${status:-0}" total_ms="$(ms "${total:-0}")"
}

# Store a resume position on the live session, then start again with
# start_position omitted and compare the returned plan's start offset with what
# the server stored. The primary session must already be stopped by the caller.
phase_resume() {
	local fid="$1" position="$2" progress_status="$3"
	local attempt body out status ttfb total rc
	attempt="e2e-${RUN_ID}-${fid}-resume"
	body="$TMPDIR/resume-${fid}.req.json"
	local rresp="$TMPDIR/resume-${fid}.json"
	build_body "$fid" "$attempt" > "$body"
	phase_begin
	out=$(post_start "$rresp" "$body" "$attempt")
	phase_end
	IFS='|' read -r status ttfb total rc <<< "$out"
	local outcome playable sid2 src_start player_start reason
	outcome=$(py_field "$rresp" outcome)
	sid2=$(py_field "$rresp" session_id)
	reason=$(py_field "$rresp" terminal.reason)
	src_start=$(py_field "$rresp" playback_plan.timeline.source_start_seconds)
	player_start=$(py_field "$rresp" playback_plan.timeline.player_start_seconds)
	playable=false
	[[ "$outcome" == "playable" && -n "$sid2" ]] && playable=true
	[[ -n "$sid2" ]] && printf '%s\n' "$sid2" >> "$SESSIONS"

	local honoured=false
	if [[ "$playable" == "true" ]]; then
		honoured=$(awk -v p="$position" -v s="${src_start:-}" -v q="${player_start:-}" \
			'BEGIN{ tol=2.0; ds=(s==""?-999:s)+0; dp=(q==""?-999:q)+0; if ((ds-p<=tol && p-ds<=tol) || (dp-p<=tol && p-dp<=tol)) print "true"; else print "false" }')
	fi
	rec resume item="$fid" attempted=true set_position="$position" progress_http_status="${progress_status:-0}" \
		attempt_id="$attempt" plan_http_status="${status:-0}" plan_ttfb_ms="$(ms "${ttfb:-0}")" \
		plan_playable="$playable" observed_source_start_seconds="${src_start:-}" \
		observed_player_start_seconds="${player_start:-}" resume_honoured="$honoured" terminal_reason="${reason:-}"
	# Do not leave the resume session behind.
	[[ -n "$sid2" ]] && curl -sS --max-time "$CURL_TIMEOUT" -X DELETE "$SERVER/api/v1/playback/$sid2" \
		-H "$AUTH" -o /dev/null 2>/dev/null || true
}

run_item() {
	local fid="$1"
	local attempt body resp out http_status ttfb total rc
	attempt="e2e-${RUN_ID}-${fid}-start"
	resp="$TMPDIR/start-${fid}.json"
	body="$TMPDIR/start-${fid}.req.json"
	build_body "$fid" "$attempt" > "$body"

	phase_begin
	out=$(post_start "$resp" "$body" "$attempt")
	phase_end
	IFS='|' read -r http_status ttfb total rc <<< "$out"
	[[ -z "$ttfb" ]] && ttfb=0
	[[ -z "$total" ]] && total=0

	local outcome sid reason err_code playable delivery proto plan_url src_start
	outcome=$(py_field "$resp" outcome)
	sid=$(py_field "$resp" session_id)
	reason=$(py_field "$resp" terminal.reason)
	err_code=$(py_field "$resp" error.code)
	delivery=$(py_field "$resp" playback_plan.delivery)
	proto=$(py_field "$resp" playback_plan.stream.protocol)
	plan_url=$(py_field "$resp" playback_plan.stream.url)
	src_start=$(py_field "$resp" playback_plan.timeline.source_start_seconds)
	playable=false
	[[ "$outcome" == "playable" && -n "$sid" && -n "$plan_url" ]] && playable=true
	[[ -n "$sid" ]] && printf '%s\n' "$sid" >> "$SESSIONS"

	local surl=""
	[[ -n "$plan_url" ]] && surl=$(sanitize_url "$(resolve_media_url "$plan_url")")
	rec start item="$fid" attempt_id="$attempt" http_status="${http_status:-0}" \
		ttfb_ms="$(ms "$ttfb")" total_ms="$(ms "$total")" outcome="${outcome:-}" \
		terminal_reason="${reason:-}" error_code="${err_code:-}" playable="$playable" \
		delivery="${delivery:-}" protocol="${proto:-}" stream_url="$surl" session_id="${sid:-}" \
		source_start_seconds="${src_start:-0}"

	if [[ "$playable" != "true" ]]; then
		rec first_bytes item="$fid" attempted=false reason=not_playable valid=false
		rec throughput item="$fid" attempted=false reason=not_playable valid=false
		rec seek item="$fid" attempted=false reason=not_playable valid=false
		rec resume item="$fid" attempted=false reason=not_playable
		phase_stop "$fid" "$sid"
		return
	fi

	phase_first_bytes "$fid" "$plan_url" "$proto"
	local total_size
	total_size=$(python3 - "$RECORDS" "$fid" <<'PY'
import json, sys
total = 0
for line in open(sys.argv[1]):
    try:
        r = json.loads(line)
    except Exception:
        continue
    if r.get("phase") == "first_bytes" and str(r.get("item")) == str(sys.argv[2]):
        total = int(r.get("total_size_bytes") or 0)
print(total)
PY
)
	local duration
	duration=$(py_field "$resp" playback_plan.source.duration_seconds)
	phase_throughput "$fid" "$delivery" "$proto" "$plan_url"
	phase_seek "$fid" "$delivery" "$proto" "$plan_url" "$total_size" "$duration"

	# Resume check: store a position on the live session, stop the session
	# (measured), then start again with start_position omitted.
	local position progress_status
	position=$(awk -v d="${duration:-0}" -v p="$RESUME_POSITION" \
		'BEGIN{ if (d+0>0) { x=d*0.5; if (x<p) printf "%.1f", x; else printf "%.1f", p } else printf "%.1f", p }')
	progress_status=$(curl -sS --max-time "$CURL_TIMEOUT" -X POST "$SERVER/api/v1/playback/$sid/progress" \
		-H "$AUTH" -H 'Content-Type: application/json' -H "X-Profile-Id: $PROFILE_ID" \
		-o /dev/null -w '%{http_code}' \
		-d "{\"position\": $position, \"is_paused\": false}" 2>/dev/null || echo "0")
	phase_stop "$fid" "$sid"
	phase_resume "$fid" "$position" "$progress_status"

	# Subtitles use their own sessions: a completed extraction can end the one
	# that requested it.
	phase_subtitles "$fid" "$resp"
}

# ── Main ──────────────────────────────────────────────────────────────────────

if [[ "$FIND_SUBTITLES" -eq 1 ]]; then
	run_find_subtitles
fi

ITEMS=()
if [[ -n "$ITEM_FILTER" ]]; then
	ITEMS+=("$ITEM_FILTER	item	item")
else
	echo "discovering candidates..."
	candidates=$(discover_candidates) || true
	if [[ -z "$candidates" ]]; then
		die "no candidates discovered from libraries $MOVIE_LIBRARY_ID/$SERIES_LIBRARY_ID; pass --item <file_id>" 3
	fi
	printf '%s\n' "$candidates" > "$TMPDIR/candidates.tsv"
	selected=$(sample_candidates "$TMPDIR/candidates.tsv")
	while IFS=$'\t' read -r cid title typ; do
		[[ -z "$cid" ]] && continue
		fid=$(resolve_file_id "$cid") || true
		[[ -z "$fid" ]] && continue
		ITEMS+=("$fid	$title	$typ")
	done <<< "$selected"
fi
[[ ${#ITEMS[@]} -gt 0 ]] || die "no items to measure; pass --item <file_id>" 3

DISCOVERED_COUNT=1
MEASURED_COUNT=${#ITEMS[@]}
if [[ -z "$ITEM_FILTER" ]]; then
	DISCOVERED_COUNT=$(wc -l < "$TMPDIR/candidates.tsv" | tr -d ' ')
fi

printf '%s\n' "${ITEMS[@]}" > "$TMPDIR/items.tsv"

GIT_REV=""
GIT_DIRTY=false
if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
	GIT_REV=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || true)
	if [[ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null | sed -n '1p')" ]]; then
		GIT_DIRTY=true
	fi
fi

META_SERVER="$(sanitize_url "$SERVER")" \
META_PROFILE="$PROFILE_ID" \
META_RUN_ID="$RUN_ID" \
META_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
META_GIT_REV="$GIT_REV" \
META_GIT_DIRTY="$GIT_DIRTY" \
META_SEED="$SEED" \
META_SAMPLE_SIZE="$SAMPLE_SIZE" \
META_SEEKS="$SEEKS" \
META_SUBTITLE_REPEATS="$SUBTITLE_REPEATS" \
META_SETTLE_MS="$SETTLE_MS" \
META_RANGE_BYTES="$RANGE_BYTES" \
META_THROUGHPUT_BYTES="$THROUGHPUT_BYTES" \
META_STALL_MS="$STALL_MS" \
META_SUBTITLE_TIMEOUT="$SUBTITLE_TIMEOUT" \
META_RESUME_POSITION="$RESUME_POSITION" \
META_SUBTITLE_ORDINAL="$SUBTITLE_ORDINAL" \
META_DISCOVERED="$DISCOVERED_COUNT" \
META_MEASURED="$MEASURED_COUNT" \
META_ITEMS_TSV="$TMPDIR/items.tsv" \
python3 - "$TMPDIR/meta.json" <<'PY'
import json, os, sys

items = []
with open(os.environ["META_ITEMS_TSV"]) as fh:
    for line in fh:
        parts = line.rstrip("\n").split("\t")
        if parts and parts[0]:
            items.append({
                "file_id": int(parts[0]) if parts[0].isdigit() else parts[0],
                "title": parts[1] if len(parts) > 1 else "",
                "type": parts[2] if len(parts) > 2 else "",
            })

meta = {
    "tool": "bench-playback-e2e",
    "server": os.environ["META_SERVER"],
    "profile_id": os.environ["META_PROFILE"],
    "run_id": os.environ["META_RUN_ID"],
    "timestamp": os.environ["META_TIMESTAMP"],
    "git_revision": os.environ["META_GIT_REV"] or None,
    "git_dirty": os.environ["META_GIT_DIRTY"] == "true",
    "seed": int(os.environ["META_SEED"]),
    "sample_size": int(os.environ["META_SAMPLE_SIZE"]),
    "seeks": int(os.environ["META_SEEKS"]),
    "subtitle_repeats": int(os.environ["META_SUBTITLE_REPEATS"]),
    "settle_ms": int(os.environ["META_SETTLE_MS"]),
    "range_bytes": int(os.environ["META_RANGE_BYTES"]),
    "throughput_bytes": int(os.environ["META_THROUGHPUT_BYTES"]),
    "stall_ms": int(os.environ["META_STALL_MS"]),
    "subtitle_timeout": int(os.environ["META_SUBTITLE_TIMEOUT"]),
    "resume_position": float(os.environ["META_RESUME_POSITION"]),
    "subtitle_ordinal": (int(os.environ["META_SUBTITLE_ORDINAL"])
                         if os.environ.get("META_SUBTITLE_ORDINAL") else None),
    "candidates_discovered": int(os.environ["META_DISCOVERED"]),
    "items_measured": int(os.environ["META_MEASURED"]),
    "items": items,
}
json.dump(meta, open(sys.argv[1], "w"), indent=2)
PY

echo "measuring ${#ITEMS[@]} item(s)..."
for entry in "${ITEMS[@]}"; do
	IFS=$'\t' read -r fid title typ <<< "$entry"
	printf '  [%s] file_id=%s %s\n' "$typ" "$fid" "$title"
	run_item "$fid"
done

if [[ -n "$JSON_OUT" ]]; then
	python3 "$TMPDIR/aggregate.py" "$RECORDS" "$TMPDIR/meta.json" "$JSON_OUT"
	echo "JSON report written to $JSON_OUT"
else
	python3 "$TMPDIR/aggregate.py" "$RECORDS" "$TMPDIR/meta.json" "$TMPDIR/report.json"
fi
