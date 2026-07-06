#!/usr/bin/env python3
"""Unit tests for backfill_language.py — Pre-Processing Regex, Token-Schutz, AQ-Merge."""

import json
import os
import sqlite3
import tempfile
import unittest
from unittest.mock import patch, MagicMock

# Import module under test
from backfill_language import (
    pre_process,
    post_process,
    translate_text,
    aq_merge_english,
    select_candidates,
    load_secrets,
    load_state,
    save_state,
    patch_state,
    BATCH_SIZE,
)

# ─── Pre-Processing Tests ───────────────────────────────────────────────────

class TestPreProcessing(unittest.TestCase):
    """Unit tests for pre_process() — each regex pattern individually."""

    def test_codefence(self):
        """Code fences ```...``` must be wrapped in <k>."""
        text = "Before\n```python\nx = 1\n```\nAfter"
        result = pre_process(text)
        self.assertIn("<k>```python\nx = 1\n```</k>", result)
        self.assertTrue(result.startswith("Before\n"))
        self.assertTrue(result.endswith("\nAfter"))

    def test_codefence_multiline(self):
        """Multi-line code fences are fully wrapped."""
        text = "Text\n```\nline 1\nline 2\nline 3\n```\nEnd"
        result = pre_process(text)
        self.assertIn("<k>```\nline 1\nline 2\nline 3\n```</k>", result)

    def test_inlinecode(self):
        """Inline code `code` must be wrapped."""
        text = "Use the `foo()` function"
        result = pre_process(text)
        self.assertIn("<k>`foo()`</k>", result)

    def test_inlinecode_multiple(self):
        """Multiple inline code spans are wrapped independently."""
        text = "Call `foo()` then `bar()`"
        result = pre_process(text)
        self.assertIn("<k>`foo()`</k>", result)
        self.assertIn("<k>`bar()`</k>", result)

    def test_filepath(self):
        """File paths /a/b/c must be wrapped."""
        text = "See /path/to/file.py for details"
        result = pre_process(text)
        self.assertIn("<k>/path/to/file.py</k>", result)

    def test_filepath_dotted(self):
        """Paths with dots like /a/b/c.d are wrapped."""
        text = "Found in /home/user/config.yaml"
        result = pre_process(text)
        self.assertIn("<k>/home/user/config.yaml</k>", result)

    def test_filepath_root(self):
        """Root-level paths /file.py must be wrapped."""
        text = "Run /script.py"
        result = pre_process(text)
        self.assertIn("<k>/script.py</k>", result)

    def test_funccall(self):
        """Function calls foo.bar() must be wrapped."""
        text = "Run the obj.method() call"
        result = pre_process(text)
        self.assertIn("<k>obj.method()</k>", result)

    def test_funccall_hyphenated(self):
        """Hyphenated names like my-func.run() are wrapped."""
        text = "Use the my-func.run() helper"
        result = pre_process(text)
        self.assertEqual(result, "Use the <k>my-func.run()</k> helper")

    def test_hexid(self):
        """Hex IDs (8+ hex chars) must be wrapped."""
        text = "Found id abc12345def67890"
        result = pre_process(text)
        self.assertIn("<k>abc12345def67890</k>", result)

    def test_hexid_uppercase(self):
        """Uppercase hex IDs (8+ chars) must be wrapped."""
        text = "Found id ABCDEF1234567890"
        result = pre_process(text)
        self.assertIn("<k>ABCDEF1234567890</k>", result)

    def test_hexid_mixed_case(self):
        """Mixed-case hex IDs must be wrapped."""
        text = "Commit aBcDeF123456"
        result = pre_process(text)
        self.assertIn("<k>aBcDeF123456</k>", result)

    def test_short_hexid_uppercase(self):
        """Short uppercase hex under 8 chars is NOT wrapped."""
        text = "Short ABCDEFG"  # 7 chars
        result = pre_process(text)
        self.assertNotIn("<k>", result)

    def test_version(self):
        """Version numbers v1.2.3 must be wrapped."""
        text = "Upgrade to v2.0.1"
        result = pre_process(text)
        self.assertIn("<k>v2.0.1</k>", result)

    def test_version_semver(self):
        """Semver v1.2.3-alpha etc (only vX.Y.Z base)."""
        text = "Release v3.0.0-beta"
        result = pre_process(text)
        self.assertIn("<k>v3.0.0</k>", result)

    def test_shellvar_dollar(self):
        """$VAR shell variables must be wrapped."""
        text = "Set $HOME variable"
        result = pre_process(text)
        self.assertIn("<k>$HOME</k>", result)

    def test_shellvar_percent(self):
        """%VAR% shell variables must be wrapped."""
        text = "Set %TEMP%"
        result = pre_process(text)
        self.assertIn("<k>%TEMP%</k>", result)

    def test_shellvar_curly_brace(self):
        """${VAR} curly brace syntax must be wrapped."""
        text = "Set ${HOME}/path"
        result = pre_process(text)
        self.assertIn("<k>${HOME}</k>", result)

    def test_triggerprefix(self):
        """deadline: prefix must be wrapped (entire line)."""
        text = "Finish before deadline: 2026-07-10"
        result = pre_process(text)
        self.assertIn("<k>deadline: 2026-07-10</k>", result)

    def test_order_codefence_before_inlinecode(self):
        """Code fences processed before inline code — no double-wrap."""
        text = "```\n`inner`\n``` and `outer`"
        result = pre_process(text)
        # The code fence wraps everything inside
        self.assertIn("<k>```\n`inner`\n```</k>", result)
        # The outer inline code is wrapped separately
        self.assertIn("<k>`outer`</k>", result)

    def test_no_double_wrap(self):
        """No <k> tag double-wrapping occurs."""
        text = "File /path/to/file.py is used"
        result = pre_process(text)
        self.assertIn("<k>/path/to/file.py</k>", result)
        self.assertEqual(result.count("<k>"), 1,
                         msg=f"Expected 1 <k>, got {result.count('<k>')}. Result: {result}")

    def test_empty_string(self):
        """Empty string returns empty."""
        self.assertEqual(pre_process(""), "")

    def test_no_tokens(self):
        """Plain text without tokens is unchanged."""
        text = "This is just normal English text."
        self.assertEqual(pre_process(text), text)

    def test_multiple_patterns(self):
        """Mixed tokens in one text."""
        text = (
            "In `setup()` check /etc/config.yaml, "
            "run obj.test() and set $DEBUG. "
            "Version v1.2.3 id a1b2c3d4e5f6a7b8. "
            "```\ncode\n```"
        )
        result = pre_process(text)
        self.assertIn("<k>`setup()`</k>", result)
        self.assertIn("<k>/etc/config.yaml</k>", result)
        self.assertIn("<k>obj.test()</k>", result)
        self.assertIn("<k>$DEBUG</k>", result)
        self.assertIn("<k>v1.2.3</k>", result)
        self.assertIn("<k>a1b2c3d4e5f6a7b8</k>", result)
        self.assertIn("<k>```\ncode\n```</k>", result)

    def test_triggerprefix_with_deadline_and_more(self):
        """deadline: prefix captures until newline."""
        text = "Do before deadline: 2026-07-10\nNext line"
        result = pre_process(text)
        self.assertIn("<k>deadline: 2026-07-10</k>", result)


class TestPostProcessing(unittest.TestCase):
    """Unit tests for post_process()."""

    def test_strip_k_tags(self):
        """<k> and </k> tags are removed."""
        result = post_process("Hello <k>world</k>")
        self.assertEqual(result, "Hello world")

    def test_multiple_tags(self):
        """Multiple <k> tags are stripped."""
        result = post_process("<k>a</k> and <k>b</k>")
        self.assertEqual(result, "a and b")

    def test_empty_string(self):
        """Empty string returns empty."""
        self.assertEqual(post_process(""), "")

    def test_no_tags(self):
        """Text without <k> tags is unchanged."""
        text = "Normal text."
        self.assertEqual(post_process(text), text)

    def test_nested_not_applicable(self):
        """<k> tags never nest; flat structure assumed."""
        result = post_process("start <k>middle</k> end")
        self.assertEqual(result, "start middle end")


# ─── DeepL-Mock Tests ───────────────────────────────────────────────────────

class TestDeepLTokenProtection(unittest.TestCase):
    """Token-Schutz: <k>-wrapped content must survive translation verbatim."""

    @patch("backfill_language.requests.post")
    def test_k_tags_preserved_in_translation(self, mock_post):
        """DeepL preserves <k> content; post_process recovers it."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {
            "translations": [{"text": "The function <k>`validate()`</k> checks the ID"}]
        }
        mock_post.return_value = mock_response

        original = "Die Funktion `validate()` prüft die ID"
        processed = pre_process(original)
        translated = translate_text(processed, "fake_key")
        cleaned = post_process(translated)

        # Technical token preserved verbatim
        self.assertIn("validate()", cleaned)
        # <k> tags removed
        self.assertNotIn("<k>", cleaned)
        self.assertNotIn("</k>", cleaned)

    @patch("backfill_language.requests.post")
    def test_multiple_protected_tokens(self, mock_post):
        """Multiple <k> tokens all survive translation."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {
            "translations": [{
                "text": (
                    "The <k>`setup()`</k> function reads <k>/etc/config.yaml</k> "
                    "and sets <k>$DEBUG</k>"
                )
            }]
        }
        mock_post.return_value = mock_response

        original = "Die `setup()` Funktion liest /etc/config.yaml und setzt $DEBUG"
        processed = pre_process(original)
        translated = translate_text(processed, "fake_key")
        cleaned = post_process(translated)

        self.assertIn("`setup()`", cleaned)
        self.assertIn("/etc/config.yaml", cleaned)
        self.assertIn("$DEBUG", cleaned)

    @patch("backfill_language.requests.post")
    def test_codefence_preserved(self, mock_post):
        """Code fence content inside <k> survives verbatim."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {
            "translations": [{
                "text": "Example:\n<k>```\nx = 1\n```</k>\nEnd"
            }]
        }
        mock_post.return_value = mock_response

        original = "Beispiel:\n```\nx = 1\n```\nEnde"
        processed = pre_process(original)
        translated = translate_text(processed, "fake_key")
        cleaned = post_process(translated)

        self.assertIn("```\nx = 1\n```", cleaned)

    @patch("backfill_language.requests.post")
    def test_translate_empty_returns_empty(self, mock_post):
        """Empty text returns empty without API call."""
        result = translate_text("", "fake_key")
        self.assertEqual(result, "")
        mock_post.assert_not_called()


# ─── AQ Merge Tests ──────────────────────────────────────────────────────────

class TestAQMerge(unittest.TestCase):
    """AQ-Merge-Logik: case-insensitive dedup, additional rows only."""

    def setUp(self):
        """Create in-memory SQLite database for testing."""
        self.conn = sqlite3.connect(":memory:")
        self.conn.execute("""
            CREATE TABLE learnings (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                content TEXT NOT NULL
            )
        """)
        self.conn.execute("""
            CREATE TABLE learning_anticipated_queries (
                learning_id INTEGER NOT NULL,
                value TEXT NOT NULL
            )
        """)
        self.conn.execute("INSERT INTO learnings (id, content) VALUES (1, 'test')")
        # Existing AQs
        existing = [
            "DeepL API usage",
            "deepl api usage",  # case-different
            "translation setup",
            "Übersetzung konfigurieren",
        ]
        for v in existing:
            self.conn.execute(
                "INSERT INTO learning_anticipated_queries (learning_id, value) VALUES (1, ?)",
                (v,),
            )
        self.conn.commit()

    def tearDown(self):
        self.conn.close()

    def test_new_aqs_inserted(self):
        """New English AQs are inserted as additional rows."""
        new_aqs = ["DeepL configuration", "API key setup"]
        aq_merge_english(self.conn, 1, new_aqs)
        rows = self.conn.execute(
            "SELECT value FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchall()
        values = [r[0] for r in rows]
        self.assertIn("DeepL configuration", values)
        self.assertIn("API key setup", values)

    def test_case_insensitive_dedup(self):
        """Case-different duplicates are skipped (no new row added)."""
        count_before = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        new_aqs = ["DEEPL API USAGE"]  # same as existing (case-insensitive)
        aq_merge_english(self.conn, 1, new_aqs)
        count_after = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        self.assertEqual(count_before, count_after,
                         msg="Duplicate AQ should not increase row count")

    def test_exact_duplicate_skipped(self):
        """Exact duplicate is not inserted again."""
        count_before = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        aq_merge_english(self.conn, 1, ["DeepL API usage"])  # exact match
        count_after = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        self.assertEqual(count_before, count_after)

    def test_mixed_dups_and_new(self):
        """Mix of duplicates and new AQs: only new ones inserted."""
        count_before = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        new_aqs = ["DEEPL API USAGE", "brand new query", "TRANSLATION SETUP"]
        aq_merge_english(self.conn, 1, new_aqs)
        count_after = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        # Only "brand new query" is new (others are case-insensitive dups)
        self.assertEqual(count_after, count_before + 1)

    def test_empty_aqs_noop(self):
        """Empty AQ list inserts nothing."""
        count_before = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        aq_merge_english(self.conn, 1, [])
        count_after = self.conn.execute(
            "SELECT COUNT(*) FROM learning_anticipated_queries WHERE learning_id = 1"
        ).fetchone()[0]
        self.assertEqual(count_before, count_after)


# ─── Candidate Selection Tests ───────────────────────────────────────────────

class TestSelectCandidates(unittest.TestCase):
    """Candidate selection logic."""

    def setUp(self):
        self.conn = sqlite3.connect(":memory:")
        self.conn.execute("""
            CREATE TABLE learnings (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                content TEXT NOT NULL,
                trigger_rule TEXT DEFAULT '',
                category TEXT NOT NULL DEFAULT 'general',
                project TEXT DEFAULT '',
                superseded_by INTEGER
            )
        """)
        # Insert test data: DE and EN learnings
        self.conn.execute("""
            INSERT INTO learnings (id, content, category, trigger_rule) VALUES
                (1, 'Ein deutsches Learning mit vielen deutschen Wörtern', 'general', ''),
                (2, 'This is an English learning about API usage', 'general', ''),
                (3, 'Die Funktion prüft die Konfiguration', 'general', ''),
                (4, 'Configure the settings correctly', 'general', ''),
                (5, 'Wir haben das Problem mit der Datenbank gelöst', 'general', '')
        """)
        self.conn.commit()

    def tearDown(self):
        self.conn.close()

    def test_selects_non_english(self):
        """Only non-EN learnings are selected."""
        candidates = select_candidates(self.conn)
        ids = [c[0] for c in candidates]
        self.assertIn(1, ids)
        self.assertIn(3, ids)
        self.assertIn(5, ids)
        self.assertNotIn(2, ids)  # EN
        self.assertNotIn(4, ids)  # EN


# ─── State Persistence Tests ─────────────────────────────────────────────────

class TestStatePersistence(unittest.TestCase):
    """State file (backfill_state.json) read/write."""

    def setUp(self):
        self.tmp = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
        self.tmp.close()

    def tearDown(self):
        if os.path.exists(self.tmp.name):
            os.unlink(self.tmp.name)

    def test_load_empty_state(self):
        """Loading non-existent state returns empty set."""
        state = load_state("/nonexistent/path.json")
        self.assertEqual(state, set())

    def test_save_and_load(self):
        """Saved state is recovered verbatim."""
        save_state({1, 2, 3}, self.tmp.name)
        loaded = load_state(self.tmp.name)
        self.assertEqual(loaded, {1, 2, 3})

    def test_save_empty(self):
        """Empty set is saved and loaded."""
        save_state(set(), self.tmp.name)
        loaded = load_state(self.tmp.name)
        self.assertEqual(loaded, set())

    def test_patch_state(self):
        """patch_state adds IDs incrementally."""
        save_state({1, 2}, self.tmp.name)
        patch_state({3, 4}, self.tmp.name)
        loaded = load_state(self.tmp.name)
        self.assertEqual(loaded, {1, 2, 3, 4})


# ─── Secrets Loading Test ────────────────────────────────────────────────────

class TestSecrets(unittest.TestCase):
    """Secrets loading from .env file."""

    def test_load_secrets_missing_file(self):
        """Non-existent file returns None, default endpoint."""
        key, endpoint = load_secrets("/nonexistent/.env")
        self.assertIsNone(key)
        self.assertEqual(endpoint, "https://api.deepl.com")

    def test_load_secrets_with_key(self):
        """Present file with key returns correctly."""
        with tempfile.NamedTemporaryFile(mode="w", suffix=".env", delete=False) as f:
            f.write("DEEPL_API_KEY=test_key_123\n")
            f.write("DEEPL_ENDPOINT=https://api.deepl.com\n")
            tmpname = f.name
        try:
            key, endpoint = load_secrets(tmpname)
            self.assertEqual(key, "test_key_123")
            self.assertEqual(endpoint, "https://api.deepl.com")
        finally:
            os.unlink(tmpname)


if __name__ == "__main__":
    unittest.main()
