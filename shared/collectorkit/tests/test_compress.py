"""Unit tests for collectorkit.compress — bronze-file compression.

Covers the write-side contract (atomic tmp+rename, verify before the
original is unlinked, mtime carry-over, cleanup on failure), the
loader-side variant resolution order (plain wins), and the transparent
readers over every variant. zstd cases skip when the optional
``zstandard`` package is absent; the gzip/plain paths are stdlib and
always run.
"""
from __future__ import annotations

import gzip
import os
from pathlib import Path

import pytest

from collectorkit import compress

zstandard = pytest.importorskip("zstandard")

BODY = b'"Type","Buy","Cur."\n"Trade","0.5","BTC"\n' * 200


def _seed(tmp_path: Path, name: str = "trades.csv") -> Path:
    f = tmp_path / name
    f.write_bytes(BODY)
    return f


# ============================================================
# compress_file
# ============================================================

def test_compress_file_roundtrip_and_removal(tmp_path):
    f = _seed(tmp_path)
    out = compress.compress_file(f)
    assert out == tmp_path / "trades.csv.zst"
    assert not f.exists()                      # original consumed
    assert not out.with_name(out.name + ".tmp").exists()
    assert zstandard.ZstdDecompressor().decompress(
        out.read_bytes(), max_output_size=1 << 20) == BODY
    assert out.stat().st_size < len(BODY) // 4  # actually compressed


def test_compress_file_keep_original(tmp_path):
    f = _seed(tmp_path)
    out = compress.compress_file(f, remove_original=False)
    assert f.exists() and out.exists()
    assert f.read_bytes() == BODY


def test_compress_file_preserves_mtime(tmp_path):
    f = _seed(tmp_path)
    past = 1_600_000_000
    os.utime(f, (past, past))
    out = compress.compress_file(f)
    assert int(out.stat().st_mtime) == past


def test_compress_file_write_failure_keeps_original(tmp_path, monkeypatch):
    f = _seed(tmp_path)

    def boom(self, src, dst, **kw):
        dst.write(b"partial")
        raise OSError("disk full")

    monkeypatch.setattr(zstandard.ZstdCompressor, "copy_stream", boom)
    with pytest.raises(OSError):
        compress.compress_file(f)
    assert f.read_bytes() == BODY              # original untouched
    assert list(tmp_path.iterdir()) == [f]     # no tmp/final debris


def test_compress_file_verify_failure_keeps_original(tmp_path, monkeypatch):
    f = _seed(tmp_path)
    monkeypatch.setattr(compress, "decompressed_sha256",
                        lambda p: ("not-the-digest", 0))
    with pytest.raises(RuntimeError, match="verification failed"):
        compress.compress_file(f)
    assert f.read_bytes() == BODY
    assert list(tmp_path.iterdir()) == [f]


def test_compress_file_overwrites_stale_twin(tmp_path):
    # Idempotent re-run after an interruption: a stale/garbage twin is
    # atomically replaced, not tripped over.
    f = _seed(tmp_path)
    (tmp_path / "trades.csv.zst").write_bytes(b"garbage")
    out = compress.compress_file(f)
    assert zstandard.ZstdDecompressor().decompress(
        out.read_bytes(), max_output_size=1 << 20) == BODY


# ============================================================
# resolve_variant
# ============================================================

def test_resolve_prefers_plain_over_compressed(tmp_path):
    f = _seed(tmp_path)
    compress.compress_file(f, remove_original=False)
    logical = tmp_path / "trades.csv"
    assert compress.resolve_variant(logical) == logical


def test_resolve_falls_back_zst_then_gz(tmp_path):
    f = _seed(tmp_path)
    compress.compress_file(f)                  # only .zst remains
    logical = tmp_path / "trades.csv"
    assert compress.resolve_variant(logical) == tmp_path / "trades.csv.zst"

    (tmp_path / "trades.csv.zst").unlink()
    with gzip.open(tmp_path / "trades.csv.gz", "wb") as fh:
        fh.write(BODY)
    assert compress.resolve_variant(logical) == tmp_path / "trades.csv.gz"


def test_resolve_missing_is_none(tmp_path):
    assert compress.resolve_variant(tmp_path / "trades.csv") is None


# ============================================================
# readers
# ============================================================

@pytest.mark.parametrize("materialise", ["plain", "zst", "gz"])
def test_open_bytes_and_text_all_variants(tmp_path, materialise):
    f = _seed(tmp_path)
    if materialise == "zst":
        f = compress.compress_file(f)
    elif materialise == "gz":
        with gzip.open(tmp_path / "trades.csv.gz", "wb") as fh:
            fh.write(BODY)
        f.unlink()
        f = tmp_path / "trades.csv.gz"

    with compress.open_bytes(f) as fh:
        assert fh.read() == BODY
    with compress.open_text(f) as fh:
        assert fh.read() == BODY.decode("utf-8")


def test_decompressed_sha256_matches_plain(tmp_path):
    from collectorkit import bronze
    f = _seed(tmp_path)
    plain_digest, plain_size = bronze.sha256_file(f)
    out = compress.compress_file(f, remove_original=False)
    assert compress.decompressed_sha256(out) == (plain_digest, plain_size)
    assert compress.decompressed_sha256(f) == (plain_digest, plain_size)
