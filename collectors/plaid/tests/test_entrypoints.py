"""The entry points build, and the wrapper hands each verb its dirs.

`--help` evaluates every argparse default, so it catches a typo in one
that an import alone would not. The wrapper runs with its data root and
secrets dir under a temp dir. Help and an unknown verb end before
anything is resolved or created. `prune`, `load`, a `login` with nothing
left open and a `download` with no Item touch only local files, so they
run for real there.
"""
import subprocess
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent.parent
WRAPPER = HERE / "plaid"


@pytest.mark.parametrize("script,flag", [
    ("link.py", "--require"), ("link.py", "--new-link"),
    ("login.py", "--check"),
    ("download.py", "--lookback"),
    ("load.py", "--force"), ("prune.py", "--min-age-hours")])
def test_each_entry_point_builds_its_help(script, flag):
    result = subprocess.run([sys.executable, str(HERE / script), "--help"],
                            capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert flag in result.stdout


def _wrapper(tmp_path, *argv, **env):
    # No terminal at stdin, however the suite is started.
    return subprocess.run(
        [str(WRAPPER), *argv], capture_output=True, text=True,
        stdin=subprocess.DEVNULL,
        env={"PATH": "/usr/bin:/bin", "HOME": str(tmp_path),
             "WEALTHDB_DATA_ROOT": str(tmp_path / "data"),
             "WEALTHDB_SECRETS_DIR": str(tmp_path / "secrets"), **env})


def _one_run(data):
    """One complete run of the Item `bank`, the least a load reads."""
    run = data / "plaid" / "bank" / "20260101T000000Z"
    run.mkdir(parents=True)
    (run / "item.json").write_text('{"item": {"item_id": "item-synthetic"}}')
    (run / "accounts.json").write_text('{"accounts": []}')
    (run / "run.json").write_text(
        '{"status": "complete", "item": "bank", "item_id": "item-synthetic",'
        ' "environment": "sandbox", "products": {"accounts": {"status":'
        ' "fetched", "rows": 0, "files": ["accounts.json"]}}}')
    return run.parent


def test_the_wrapper_prints_help_for_no_arguments(tmp_path):
    for argv in ((), ("help",), ("--help",), ("-h",)):
        result = _wrapper(tmp_path, *argv)
        assert result.returncode == 0, result.stderr
        for verb in ("link --item NAME", "login --check", "download",
                     "load", "prune"):
            assert verb in result.stdout
        assert "<data-dir>/<item>/<item>.db" in result.stdout
    assert not (tmp_path / "data").exists()


def test_the_wrapper_refuses_a_verb_it_does_not_have(tmp_path):
    result = _wrapper(tmp_path, "transfer")
    assert result.returncode == 2
    assert "unknown command 'transfer'" in result.stderr
    assert not (tmp_path / "data").exists()


def test_link_without_an_item_runs_nothing(tmp_path):
    # No arguments must never start an action: here that means no link.
    result = _wrapper(tmp_path, "link", PLAID_CLIENT_ID="synthetic-id",
                      PLAID_SECRET="synthetic-secret")
    assert result.returncode == 2
    assert "--item NAME is required" in result.stderr
    assert not (tmp_path / "secrets").exists()


def test_link_without_the_keys_says_what_to_set_up(tmp_path):
    # The setup check of setup.txt answers before link.py runs.
    result = _wrapper(tmp_path, "link", "--item", "bank")
    assert result.returncode == 78
    assert "PLAID_CLIENT_ID='<client-id>'" in result.stderr
    assert "Then run: wealthdb-collect plaid link" in result.stderr
    assert not (tmp_path / "secrets").exists()


def test_a_new_production_link_with_no_terminal_ends_before_plaid(tmp_path):
    # No one can answer the question before a new production link, so
    # the run ends there. The proxy is a closed port: a call to Plaid
    # would fail on it and end the run another way.
    closed = "http://127.0.0.1:9"
    (tmp_path / "secrets").mkdir(mode=0o700)
    result = _wrapper(tmp_path, "link", "--item", "bank",
                      PLAID_CLIENT_ID="synthetic-id",
                      PLAID_SECRET="synthetic-secret",
                      HTTPS_PROXY=closed, https_proxy=closed)
    assert result.returncode == 2, result.stderr
    assert ("plaid: a new production link needs a yes. Run this in a "
            "terminal, or pass --new-link.") in result.stderr
    assert result.stdout == ""
    assert not list((tmp_path / "secrets").glob("plaid-*.json"))


def test_login_with_no_item_names_link_first(tmp_path):
    result = _wrapper(tmp_path, "login")
    assert result.returncode == 0, result.stderr
    assert "wealthdb-collect plaid link --item <name>" in result.stderr


def test_a_setup_of_its_own_names_its_config_in_the_first_step(tmp_path):
    # The words the launcher hands a setup of its own reach the texts of
    # the setup declaration.
    words = "wealthdb -c /x/try/wealthdb.cfg collect"
    result = _wrapper(tmp_path, "login", WEALTHDB_COLLECT=words)
    assert result.returncode == 0, result.stderr
    assert f"{words} plaid link --item <name>" in result.stderr
    assert "wealthdb-collect" not in result.stderr
    result = _wrapper(tmp_path, "link", "--item", "bank",
                      WEALTHDB_COLLECT=words, WEALTHDB_PROGRAM="wealthdb -c /x/try/wealthdb.cfg")
    assert result.returncode == 78
    assert f"Then run: {words} plaid link --item bank" in result.stderr
    assert "wealthdb -c /x/try/wealthdb.cfg load -a" in result.stderr


def test_a_setup_of_its_own_keeps_its_data_in_its_folder(tmp_path):
    # A config that is not the default one: the data folder is in the
    # config's folder, whatever WEALTHDB_DATA_ROOT names.
    setup = tmp_path / "try"
    words = f"wealthdb -c {setup}/wealthdb.cfg collect"
    result = _wrapper(tmp_path, "load", WEALTHDB_CONFIG=f"{setup}/wealthdb.cfg",
                      WEALTHDB_COLLECT=words)
    assert result.returncode == 0, result.stderr
    assert (f"no Item tree under {setup}/plaid; nothing to load. "
            f"Download first: {words} plaid download") in result.stderr
    assert (setup / "plaid").is_dir()
    assert not (tmp_path / "data").exists()


def test_login_with_nothing_left_open_is_a_clean_no_op(tmp_path):
    # What an orchestrator's login -> download -> load runs first. It needs
    # no app keys and asks Plaid nothing when no sign-in is left open.
    result = _wrapper(tmp_path, "login")
    assert result.returncode == 0, result.stderr
    assert "No production sign-in is left open." in result.stdout
    assert not (tmp_path / "secrets").exists()


@pytest.mark.parametrize("verb", ["link", "login", "download", "prune"])
def test_the_silver_db_flag_is_refused_where_nothing_reads_it(tmp_path,
                                                               verb):
    result = _wrapper(tmp_path, verb, "--silver-db", str(tmp_path / "x.db"))
    assert result.returncode == 2
    assert "--silver-db does not apply" in result.stderr


def test_prune_runs_over_the_data_dir(tmp_path):
    # Local files only, so the wrapper can be run for real here.
    tree = tmp_path / "data" / "plaid" / "bank" / "20260101T000000Z"
    tree.mkdir(parents=True)
    (tree / "run.json").write_text('{"status": "complete", "item": "bank"}')
    result = _wrapper(tmp_path, "prune", "--dry-run")
    assert result.returncode == 0, result.stderr
    assert "== bank" in result.stdout and "nothing to prune" in result.stdout


def test_download_runs_over_the_data_dir(tmp_path):
    # With no Item stored, download ends before it asks Plaid anything.
    (tmp_path / "secrets").mkdir(mode=0o700)
    result = _wrapper(tmp_path, "download", "--sandbox")
    assert result.returncode == 1, result.stderr
    assert "no sandbox Item is linked" in result.stderr
    assert (tmp_path / "data" / "plaid").is_dir()
    assert not any((tmp_path / "data" / "plaid").iterdir())


def test_load_runs_over_the_data_dir(tmp_path):
    # Local files only. No Item tree yet: nothing to load, and no silver.
    result = _wrapper(tmp_path, "load")
    assert result.returncode == 0, result.stderr
    assert "nothing to load" in result.stderr
    assert not any((tmp_path / "data" / "plaid").iterdir())
    # One complete run of an Item: its silver lands in its own tree.
    tree = _one_run(tmp_path / "data")
    result = _wrapper(tmp_path, "load", "--item", "bank")
    assert result.returncode == 0, result.stderr
    assert (tree / "bank.db").is_file()
    assert not (tmp_path / "data" / "plaid" / "plaid.db").exists()


def test_load_takes_a_silver_path_for_one_named_item_only(tmp_path):
    # The shared default, one <data-dir>/plaid.db, never applies: each
    # Item has its own database, so a path given needs the Item it is for.
    result = _wrapper(tmp_path, "load", "--silver-db", str(tmp_path / "x.db"))
    assert result.returncode == 2
    assert "names one Item's database" in result.stderr


def test_a_silver_path_from_the_environment_needs_one_item_too(tmp_path):
    result = _wrapper(tmp_path, "load", PLAID_SILVER_DB=str(tmp_path / "x.db"))
    assert result.returncode == 2
    assert "PLAID_SILVER_DB" in result.stderr


@pytest.mark.parametrize("how", ["flag", "flag=", "env"])
def test_a_silver_path_given_to_the_wrapper_reaches_load(tmp_path, how):
    tree = _one_run(tmp_path / "data")
    target = tmp_path / "elsewhere.db"
    argv, env = ["load", "--item", "bank"], {}
    if how == "flag":
        argv += ["--silver-db", str(target)]
    elif how == "flag=":
        argv += [f"--silver-db={target}"]
    else:
        env["PLAID_SILVER_DB"] = str(target)
    result = _wrapper(tmp_path, *argv, **env)
    assert result.returncode == 0, result.stderr
    assert target.is_file()
    assert not (tree / "bank.db").exists()
