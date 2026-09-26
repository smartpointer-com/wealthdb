"""collectorkit.parser_cli — a statement parser's standalone command line.
The parse function is a stand-in; no PDF is opened."""
import json

from collectorkit import parser_cli


def _parse(path, **kwargs):
    return {"path": path, **kwargs}


def test_results_go_to_stdout_as_one_array(capsys):
    rc = parser_cli.dump_json(["a.pdf", "b.pdf"], _parse, description="d")
    assert rc == 0
    assert json.loads(capsys.readouterr().out) == [{"path": "a.pdf"}, {"path": "b.pdf"}]


def test_json_out_writes_a_file(tmp_path, capsys):
    out = tmp_path / "out.json"
    parser_cli.dump_json(["a.pdf", "--json-out", str(out)], _parse, description="d")
    assert json.loads(out.read_text(encoding="utf-8")) == [{"path": "a.pdf"}]
    assert capsys.readouterr().out == ""


def test_a_parser_s_own_option_reaches_the_parse(capsys):
    parser_cli.dump_json(
        ["a.pdf", "--tag", "x", "--tag", "y"], _parse, description="d",
        add_arguments=lambda p: p.add_argument("--tag", action="append"),
        parse_kwargs=lambda args: {"tags": tuple(args.tag or ())})
    assert json.loads(capsys.readouterr().out) == [{"path": "a.pdf", "tags": ["x", "y"]}]


def test_values_json_cannot_hold_are_written_as_text(capsys):
    from datetime import date
    parser_cli.dump_json(["a.pdf"], lambda p: {"on": date(2098, 1, 2)}, description="d")
    assert json.loads(capsys.readouterr().out) == [{"on": "2098-01-02"}]
