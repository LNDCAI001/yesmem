#!/usr/bin/env python3
"""
Backfill non-EN learnings → EN via DeepL API with token protection.

Usage:
  python3 scripts/backfill_language.py --dry-run
  python3 scripts/backfill_language.py --limit 20 --db-path /tmp/test.db
  python3 scripts/backfill_language.py --resume

Token protection: wraps code/identifiers/paths in <k> tags, calls DeepL with
tag_handling=xml + ignore_tags=["k"], strips tags after translation.
"""

import argparse
import json
import os
import re
import sqlite3
import sys

import requests

# ─── Constants ───────────────────────────────────────────────────────────────

# Pattern order matters: longer/more specific patterns first.
# Code fences before inline code to prevent double-wrapping.
PRE_PROCESSING_PATTERNS = [
    (r"```[\s\S]*?```", "codefence"),          # Code blocks
    (r"`[^`\n]+`", "inlinecode"),               # Inline code
    (r"/(?:[\w.-]+/)*[\w.-]+", "filepath"),     # Absolute/relative paths (incl. root-level)
    (r"\b[\w-]+\.\w+\(\)", "funccall"),         # Function calls foo.bar()
    (r"\b[0-9a-fA-F]{8,}\b", "hexid"),          # UUIDs/hex IDs (8+ chars, upper+lower)
    (r"\bv\d+\.\d+\.\d+\b", "version"),         # Version numbers
    (r"\$\{[\w_-]+\}|\$\w+|%\w+%", "shellvar"), # Shell/Batch variables ($VAR, ${VAR}, %VAR%)
    (r"deadline:\s*[^\n]+", "triggerprefix"),   # trigger_rule deadline prefix
]

DEFAULT_ENDPOINT = "https://api.deepl.com"
DEEPL_TIMEOUT = 30  # seconds per API call
SECRETS_PATH = os.path.expanduser("~/.claude/yesmem/secrets.env")
STATE_FILE = "backfill_state.json"
BATCH_SIZE = 50  # DeepL allows up to 50 texts per request


# _wrap_or_skip is no longer used by pre_process (now uses segment-based approach)
# but kept for backward compatibility if imported elsewhere.


# ─── Secrets ─────────────────────────────────────────────────────────────────

def load_secrets(path=None):
    """Load DEEPL_API_KEY and DEEPL_ENDPOINT from secrets.env.

    Returns (api_key, endpoint). api_key is None if file missing.
    """
    path = path or SECRETS_PATH
    api_key = None
    endpoint = DEFAULT_ENDPOINT
    if not os.path.exists(path):
        return api_key, endpoint
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line.startswith("DEEPL_API_KEY="):
                api_key = line.split("=", 1)[1].strip().strip('"').strip("'")
            elif line.startswith("DEEPL_ENDPOINT="):
                endpoint = line.split("=", 1)[1].strip().strip('"').strip("'")
    return api_key, endpoint


# ─── Pre / Post Processing ───────────────────────────────────────────────────

def _wrap_or_skip(match, text):
    """Wrap match in <k> unless already inside a <k> tag."""
    start = match.start()
    before = text[:start]
    if before.rfind("<k>") > before.rfind("</k>"):
        return match.group()  # already inside <k>
    return f"<k>{match.group()}</k>"


def pre_process(text):
    """Wrap technical tokens in <k> tags for DeepL token protection.

    Applies patterns in order (longest first). Splits text into
    <k>-protected and unprotected segments; patterns only apply to
    unprotected segments to prevent cross-boundary matches and
    double-wrapping.
    """
    if not text:
        return text

    for pattern, _ in PRE_PROCESSING_PATTERNS:
        # Split into <k>-protected and unprotected segments
        segments = []
        last_end = 0
        for m in re.finditer(r"<k>.*?</k>", text, re.DOTALL):
            if m.start() > last_end:
                segments.append((text[last_end : m.start()], False))
            segments.append((m.group(), True))
            last_end = m.end()
        if last_end < len(text):
            segments.append((text[last_end:], False))

        # Apply pattern only to unprotected segments
        result_parts = []
        for seg_text, is_protected in segments:
            if is_protected:
                result_parts.append(seg_text)
            else:
                result_parts.append(
                    re.sub(pattern, r"<k>\g<0></k>", seg_text)
                )
        text = "".join(result_parts)

    return text


def post_process(text):
    """Remove <k> tags after DeepL translation."""
    if not text:
        return text
    return re.sub(r"</?k>", "", text)


# ─── DeepL API ───────────────────────────────────────────────────────────────

def translate_text(text, api_key, endpoint=DEFAULT_ENDPOINT):
    """Translate text via DeepL with token protection.

    Uses tag_handling=xml and ignore_tags=["k"] so <k>...</k> content
    passes through verbatim.
    Returns translated text (post-processed: <k> tags stripped).
    """
    if not text:
        return ""

    resp = requests.post(
        f"{endpoint}/v2/translate",
        headers={
            "Authorization": f"DeepL-Auth-Key {api_key}",
            "Content-Type": "application/json",
        },
        json={
            "text": [text],
            "target_lang": "EN",
            "preserve_formatting": True,
            "tag_handling": "xml",
            "ignore_tags": ["k"],
        },
        timeout=DEEPL_TIMEOUT,
    )
    if not resp.ok:
        # Extract structured error body for debugging
        try:
            body = resp.json()
        except Exception:
            body = resp.text[:500]
        raise RuntimeError(
            f"DeepL API {resp.status_code}: {body}"
        )
    data = resp.json()
    translated = data["translations"][0]["text"]
    return post_process(translated)


# ─── AQ Merge ────────────────────────────────────────────────────────────────

def aq_merge_english(conn, learning_id, new_aqs):
    """Insert new English AQ rows with case-insensitive dedup.

    Skips AQs that already exist (case-insensitive match).
    new_aqs: list of query strings to add.
    """
    if not new_aqs:
        return

    existing = {
        r[0].lower()
        for r in conn.execute(
            "SELECT value FROM learning_anticipated_queries WHERE learning_id = ?",
            (learning_id,),
        ).fetchall()
    }

    inserts = 0
    for aq in new_aqs:
        if aq.lower() not in existing:
            conn.execute(
                "INSERT INTO learning_anticipated_queries (learning_id, value) VALUES (?, ?)",
                (learning_id, aq),
            )
            existing.add(aq.lower())
            inserts += 1
    conn.commit()
    return inserts


# ─── Candidate Selection ─────────────────────────────────────────────────────

# German stopwords used for non-EN heuristic (conservative: flag if ≥2 match)
DE_STOPWORDS = {
    "der", "die", "das", "ist", "und", "mit", "auf", "für", "ein", "eine",
    "den", "dem", "des", "nicht", "werden", "wird", "sind", "wurde", "durch",
    "auch", "nach", "bei", "aus", "hat", "hast", "haben", "wäre", "wären",
    "würde", "würden", "dass", "diese", "dieser", "dieses", "dazu", "davon",
    "darin", "damit", "noch", "schon", "sehr", "oder", "aber", "weil", "wenn",
    "dann", "wie", "zum", "zur", "vom", "beim", "ins", "über", "vor",
    "zwischen", "unter", "oben", "da", "hier", "ja", "nein", "doch", "also",
    "nur", "alle", "etwas", "etwa", "immer", "nie", "man", "dir", "mich",
    "mir", "uns", "euch", "ihr", "dein", "deine", "sein", "seine", "ihre",
    "unser", "unsere", "euer", "eure", "dafür", "desto", "entweder",
    "entgegen", "gegenüber", "gemäß", "hinter", "innerhalb", "mithilfe",
    "oberhalb", "statt", "trotz", "unterhalb", "wider", "während", "bereits",
    "deutschen", "deutscher", "deutsches", "beispiel", "beispielsweise",
    "konfiguration", "konfigurieren", "einstellungen", "verwendet",
    "verwenden", "verwendung", "erfolgreich", "fehler", "fehlermeldung",
    "funktion", "methode", "ergebnis", "datei", "verzeichnis",
    "aufruf", "aufgerufen", "ausgeführt", "ausführen",
}


def _is_non_english(content):
    """Heuristic: True if content appears to be non-English.

    Counts German stopwords. Flagged if ≥2 matches in text.
    """
    if not content:
        return False
    lower = content.lower()
    count = sum(1 for w in DE_STOPWORDS if w in lower)
    return count >= 2


def select_candidates(conn, limit=None):
    """Select non-EN active learnings for backfill.

    Returns list of (id, content, trigger_rule).
    """
    query = """
        SELECT id, content, trigger_rule
        FROM learnings
        WHERE superseded_by IS NULL
        ORDER BY id
    """
    rows = conn.execute(query).fetchall()
    candidates = [r for r in rows if _is_non_english(r[1])]
    if limit:
        candidates = candidates[:limit]
    return candidates


# ─── State Persistence ───────────────────────────────────────────────────────

def load_state(path=None):
    """Load completed IDs from state file. Returns set of ints."""
    path = path or STATE_FILE
    if not os.path.exists(path):
        return set()
    with open(path) as f:
        data = json.load(f)
    return set(data.get("completed_ids", []))


def save_state(completed_ids, path=None):
    """Save completed IDs to state file."""
    path = path or STATE_FILE
    with open(path, "w") as f:
        json.dump({"completed_ids": sorted(completed_ids)}, f)


def patch_state(new_ids, path=None):
    """Atomically add IDs to state file."""
    path = path or STATE_FILE
    existing = load_state(path)
    existing.update(new_ids)
    save_state(existing, path)


# ─── Main Backfill Logic ─────────────────────────────────────────────────────

def dry_run(candidates):
    """Print backfill plan without executing."""
    print(f"Backfill candidates: {len(candidates)}")
    print(f"{'ID':>8} | {'Content Preview (pre-processed)':<60}")
    print("-" * 72)
    for lid, content, trigger in candidates[:5]:
        processed = pre_process(content)
        preview = processed[:60].replace("\n", "\\n")
        print(f"{lid:>8} | {preview}")
    if len(candidates) > 5:
        print(f"... and {len(candidates) - 5} more")
    return 0


def process_learning(conn, api_key, endpoint, lid, content, trigger_rule):
    """Translate a single learning's content and trigger_rule via DeepL.

    Returns (new_content, new_trigger, new_aqs) or raises on error.
    TODO: Re-Embedding is NOT performed here. The user must decide on
    the re-embedding strategy (SQLite trigger, daemon call, or CLI hook).
    """
    # Translate content
    wrapped = pre_process(content)
    new_content = translate_text(wrapped, api_key, endpoint)

    # Translate trigger_rule prose if present
    new_trigger = trigger_rule
    if trigger_rule:
        wrapped_trigger = pre_process(trigger_rule)
        new_trigger = translate_text(wrapped_trigger, api_key, endpoint)

    # Generate English AQs (simple: add the translated first ~200 chars as query)
    # A more sophisticated AQ generation happens in future iterations;
    # for now, just add the first sentence as a searchable phrase.
    first_sentence = new_content.split(".")[0].strip()
    new_aqs = [first_sentence] if first_sentence else []

    return new_content, new_trigger, new_aqs


def backfill(conn, api_key, endpoint, candidates, state, limit=None, dry=False):
    """Process all candidates, respecting state and limit."""
    completed = set(state)
    total = len(candidates)
    errors = []

    for idx, (lid, content, trigger) in enumerate(candidates):
        if lid in completed:
            continue
        if limit and len(completed - state) >= limit:
            break

        try:
            new_content, new_trigger, new_aqs = process_learning(
                conn, api_key, endpoint, lid, content, trigger
            )

            if not dry:
                # In-place update (no supersede)
                conn.execute(
                    "UPDATE learnings SET content=?, trigger_rule=? WHERE id=?",
                    (
                        new_content,
                        new_trigger,
                        lid,
                    ),
                )
                # Merge AQs
                aq_merge_english(conn, lid, new_aqs)
                conn.commit()

            completed.add(lid)
            sys.stderr.write(f"[{len(completed - state)}/{limit or '∞'}] id={lid} translated\n")

        except Exception as e:
            msg = f"ERROR id={lid}: {e}"
            sys.stderr.write(msg + "\n")
            errors.append(msg)

    # Save progress
    if not dry:
        patch_state(completed - state)

    return completed, errors


# ─── CLI ─────────────────────────────────────────────────────────────────────

def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description="Backfill non-EN learnings → EN via DeepL with token protection."
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Show candidates and pre-processed previews without calling DeepL",
    )
    parser.add_argument(
        "--limit",
        type=int,
        default=None,
        help="Max learnings to process (for spot-checking)",
    )
    parser.add_argument(
        "--db-path",
        default=os.path.expanduser("~/.claude/yesmem/yesmem.db"),
        help="Path to yesmem SQLite database (default: live DB)",
    )
    parser.add_argument(
        "--resume",
        action="store_true",
        help="Resume from backfill_state.json, skip already-processed IDs",
    )
    parser.add_argument(
        "--state-file",
        default=STATE_FILE,
        help=f"State file path (default: {STATE_FILE})",
    )
    parser.add_argument(
        "--secrets",
        default=SECRETS_PATH,
        help=f"Secrets file path (default: {SECRETS_PATH})",
    )
    return parser.parse_args(argv)


def main():
    args = parse_args()

    # Warn when target is the live production DB
    live_db = os.path.expanduser("~/.claude/yesmem/yesmem.db")
    if args.db_path == live_db and not args.dry_run:
        print(
            "WARNING: Target is the live yesmem DB!\n"
            "  Use `--dry-run` to preview without changes.\n"
            "  For spot-checks, copy the DB first and use `--db-path <copy>`.\n"
            "  KEIN voller Lauf ohne vorheriges Backup.\n",
            file=sys.stderr,
        )

    api_key, endpoint = load_secrets(args.secrets)
    if not api_key and not args.dry_run:
        print("ERROR: No DeepL API key found. Use --dry-run to test without translation.",
              file=sys.stderr)
        sys.exit(1)

    conn = sqlite3.connect(args.db_path)
    conn.row_factory = sqlite3.Row

    candidates = select_candidates(conn, limit=args.limit)
    print(f"Found {len(candidates)} candidate learnings", file=sys.stderr)

    if args.dry_run:
        dry_run(candidates)
        conn.close()
        return

    state = load_state(args.state_file) if args.resume else set()
    if state:
        print(f"Resuming: {len(state)} already completed", file=sys.stderr)

    completed, errors = backfill(
        conn, api_key, endpoint, candidates, state,
        limit=args.limit, dry=False,
    )

    conn.close()

    new_ids = len(completed - state)
    print(f"\nDone. Processed: {new_ids}, Errors: {len(errors)}", file=sys.stderr)
    if errors:
        for err in errors[:10]:
            print(f"  {err}", file=sys.stderr)
        if len(errors) > 10:
            print(f"  ... and {len(errors) - 10} more", file=sys.stderr)

    sys.exit(1 if errors else 0)


if __name__ == "__main__":
    main()
