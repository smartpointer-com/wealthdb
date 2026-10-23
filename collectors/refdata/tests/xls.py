"""An invented legacy Excel workbook (.xls): BIFF8 records in an OLE2
compound file, as Excel writes them, with one sheet of text, number and
date cells. The smallest file a reader of JPX's lists opens."""

import struct
from datetime import date

SECTOR = 512
END_OF_CHAIN, FREE, FAT_SECTOR, NO_STREAM = -2, -1, -3, -1
# A stream this long or longer lies in whole sectors, not in the
# compound file's mini stream.
MINI_CUTOFF = 4096
# The cell formats the workbook declares: General, and Excel's built-in
# day format (14), which a date cell names.
GENERAL_XF, DATE_XF = 0, 1
EXCEL_EPOCH = date(1899, 12, 30)


def _record(code: int, data: bytes = b"") -> bytes:
    assert len(data) <= 8224, "a BIFF record holds 8224 bytes at most"
    return struct.pack("<HH", code, len(data)) + data


def _bof(kind: int) -> bytes:
    return _record(0x0809, struct.pack("<HHHHII", 0x0600, kind, 0x0DBB, 1996, 0, 6))


def _string(s: str, lenlen: str = "H") -> bytes:
    return struct.pack(f"<{lenlen}B", len(s), 1) + s.encode("utf-16-le")


def _xf(fmt: int) -> bytes:
    return _record(0x00E0, struct.pack("<HHHBBBBIIH", 0, fmt, 0, 0x20, 0, 0, 0, 0, 0, 0x20C0))


def _workbook_stream(rows) -> bytes:
    """The Workbook stream: the globals (code page, date system, cell
    formats, the sheet's place, the shared strings), then the sheet."""
    strings = list(dict.fromkeys(v for r in rows for v in r if isinstance(v, str) and v != ""))
    cells = b""
    for r, values in enumerate(rows):
        for c, v in enumerate(values):
            if isinstance(v, date):
                cells += _record(0x0203, struct.pack("<HHHd", r, c, DATE_XF, (v - EXCEL_EPOCH).days))
            elif isinstance(v, (int, float)):
                cells += _record(0x0203, struct.pack("<HHHd", r, c, GENERAL_XF, v))
            elif v != "":
                cells += _record(0x00FD, struct.pack("<HHHI", r, c, GENERAL_XF, strings.index(v)))
    width = max(map(len, rows), default=0)
    sheet = (_bof(0x0010) + _record(0x0200, struct.pack("<IIHHH", 0, len(rows), 0, width, 0)) + cells
             + _record(0x000A))

    def globals_(sheet_at: int) -> bytes:
        return (_bof(0x0005) + _record(0x0042, struct.pack("<H", 1200)) + _record(0x0022, struct.pack("<H", 0))
                + _xf(0) + _xf(14) + _record(0x0085, struct.pack("<IBB", sheet_at, 0, 0) + _string("Sheet1", "B"))
                + _record(0x00FC, struct.pack("<II", len(strings), len(strings)) + b"".join(map(_string, strings)))
                + _record(0x000A))
    return globals_(len(globals_(0))) + sheet


def _compound(stream: bytes) -> bytes:
    """`stream` as the Workbook stream of an OLE2 compound file of
    512-byte sectors: the stream's sectors, one directory sector, one
    allocation table sector."""
    stream = stream.ljust(MINI_CUTOFF, b"\0")
    n = -(-len(stream) // SECTOR)
    assert n + 2 <= SECTOR // 4, "the workbook fits one allocation table sector"
    fat = [*range(1, n), END_OF_CHAIN, END_OF_CHAIN, FAT_SECTOR]
    fat += [FREE] * (SECTOR // 4 - len(fat))

    def entry(name, kind, child=NO_STREAM, start=END_OF_CHAIN, size=0):
        raw = (name + "\0").encode("utf-16-le") if name else b""
        return struct.pack("<64sHBBiii16sIQQiII", raw, len(raw), kind, 1, NO_STREAM, NO_STREAM, child, b"", 0, 0, 0,
                           start, size, 0)
    directory = entry("Root Entry", 5, child=1) + entry("Workbook", 2, start=0, size=len(stream)) + entry("", 0) * 2
    header = struct.pack("<8s16sHHHHH6sIIiIIiIiI", bytes.fromhex("D0CF11E0A1B11AE1"), b"", 0x3E, 3, 0xFFFE, 9, 6,
                         b"", 0, 1, n, 0, MINI_CUTOFF, END_OF_CHAIN, 0, END_OF_CHAIN, 0)
    header += struct.pack("<109i", n + 1, *[FREE] * 108)
    return header + stream.ljust(n * SECTOR, b"\0") + directory + struct.pack(f"<{SECTOR // 4}i", *fat)


def workbook(rows) -> bytes:
    """A one-sheet .xls of `rows`: each cell a str (text, "" for none),
    an int or float (a number) or a date (a number in a day format)."""
    return _compound(_workbook_stream([list(r) for r in rows]))
