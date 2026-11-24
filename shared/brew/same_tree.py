"""same_tree.py OURS THEIRS: do two tar archives hold the same tree?

OURS is what `git archive` makes of a tag. THEIRS is an archive that
was downloaded for that tag, with every member in one top folder, as
GitHub's archives have. The answer is yes when THEIRS holds the names
OURS holds and no other, each of the same kind: a file with the same
bytes and execute bit, a link with the same target, a folder.

Nothing is unpacked. A link in THEIRS is therefore never followed, a
name that is in THEIRS twice is seen, and no file system decides which
two names are one. A member that is no file, folder or link, or whose
name is not a plain path below the top folder, is a difference of its
own.

Exit status 0 for the same tree; 1 with one line per difference, the
first ten, otherwise; 2 for a usage error.
"""

import hashlib
import sys
import tarfile

SHOWN = 10


def shown(name):
    """A member's name with nothing in it that a terminal acts on."""
    return name.encode("unicode_escape").decode("ascii")


def read(path, top_folder):
    """The tree of one archive, and what is wrong with the archive itself.

    The tree maps each name to its kind: ("dir",), ("link", target) or
    ("file", has an execute bit, SHA-256 of its bytes). With top_folder,
    every member sits in one top folder, which is dropped from the names.
    """
    tree, wrong, tops = {}, [], set()
    with tarfile.open(path) as archive:
        for member in archive:
            name = member.name
            if top_folder:
                top, _, name = name.partition("/")
                tops.add(top)
                if not name:
                    if not member.isdir():
                        wrong.append(
                            f"{shown(member.name)}: a member outside its top folder"
                        )
                    continue
            parts = name.split("/")
            if any(part in ("", ".", "..") for part in parts):
                wrong.append(
                    f"{shown(member.name)}: not a plain path below the top folder"
                )
                continue
            if member.isdir():
                kind = ("dir",)
            elif member.issym():
                kind = ("link", member.linkname)
            elif member.isreg():
                digest = hashlib.sha256()
                with archive.extractfile(member) as data:
                    for block in iter(lambda: data.read(1 << 20), b""):
                        digest.update(block)
                kind = ("file", bool(member.mode & 0o100), digest.hexdigest())
            else:
                wrong.append(f"{shown(name)}: neither a file, a folder nor a link")
                continue
            if name in tree:
                wrong.append(f"{shown(name)}: in the archive twice")
            tree[name] = kind
            # A folder is there with or without a member of its own.
            for depth in range(1, len(parts)):
                tree.setdefault("/".join(parts[:depth]), ("dir",))
    if len(tops) > 1:
        wrong.append("a member outside its top folder")
    return tree, wrong


def differences(ours, theirs):
    out = []
    for name in sorted(ours.keys() | theirs.keys()):
        mine, other = ours.get(name), theirs.get(name)
        if mine == other:
            continue
        if other is None:
            out.append(f"{shown(name)}: missing from the archive")
        elif mine is None:
            out.append(f"{shown(name)}: not in the tag")
        elif mine[0] != other[0]:
            out.append(
                f"{shown(name)}: a {other[0]} in the archive, a {mine[0]} in the tag"
            )
        elif mine[0] == "link":
            out.append(f"{shown(name)}: the link names another target")
        elif mine[2] != other[2]:
            out.append(f"{shown(name)}: other bytes")
        else:
            out.append(f"{shown(name)}: another execute bit")
    return out


def main(argv):
    if len(argv) != 3:
        print("usage: same_tree.py OURS THEIRS", file=sys.stderr)
        return 2
    try:
        ours, wrong_ours = read(argv[1], top_folder=False)
        theirs, wrong = read(argv[2], top_folder=True)
    except (tarfile.TarError, OSError, EOFError) as e:
        print(f"an archive cannot be read: {e}")
        return 1
    found = (
        [f"the tag's own archive: {w}" for w in wrong_ours]
        + wrong
        + differences(ours, theirs)
    )
    for line in found[:SHOWN]:
        print(line)
    if len(found) > SHOWN:
        print(f"and {len(found) - SHOWN} more")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
