"""The Trader API client built from the stored token file.

`download` and `login --check` build it the same way. schwab-py reads
the token file, refreshes the 30-minute access token from the 7-day
refresh token when a request needs one, and writes the refreshed bundle
back to the file. No browser, no MFA. Both explain an OAuth rejection
the same way too.

schwab-py and authlib are imported lazily, so `--help` works on a
checkout without dependencies. The login image ships this module beside
login.py.
"""

from __future__ import annotations

from pathlib import Path


def client_from_token_file(token_path: Path, client_id: str,
                           client_secret: str):
    """A schwab-py client on the token file at `token_path`.

    schwab-py's `api_key` / `app_secret` parameters take the OAuth
    Client ID and Client Secret that Schwab issues in its developer
    portal; the names do not match Schwab's own terms. Building the
    client or making its first request can force a token refresh, which
    is where an expired or revoked refresh token surfaces as one of
    :func:`oauth_error_types`.
    """
    try:
        import schwab
    except ImportError:
        raise SystemExit(
            "schwab-py is not installed. Run: pip install -r requirements.txt"
        ) from None
    return schwab.auth.client_from_token_file(
        token_path=str(token_path),
        api_key=client_id,
        app_secret=client_secret,
    )


def oauth_error_types() -> tuple[type, ...]:
    """authlib OAuth error classes to catch, or () if authlib is absent
    (it ships transitively with schwab-py)."""
    types: list[type] = []
    try:
        from authlib.integrations.base_client.errors import OAuthError
        types.append(OAuthError)
    except ImportError:
        pass
    try:
        from authlib.oauth2.rfc6749.errors import OAuth2Error
        types.append(OAuth2Error)
    except ImportError:
        pass
    return tuple(types)


def explain_token_failure(token_path: Path, err: Exception) -> str:
    """Turn an authlib OAuth refresh failure into an actionable message.

    `str(err)` carries Schwab's OAuth response (e.g. invalid_grant /
    'Refresh token is invalid, expired or revoked') — a generic OAuth
    error with no account data, safe to surface verbatim."""
    detail = str(err).strip()
    lines = [
        "Schwab rejected the stored OAuth token — cannot authenticate.",
        f"  Schwab said: {detail}",
    ]
    if "invalid_client" in detail.lower():
        lines.append(
            "This is a client-credential problem, not the token: check "
            "SCHWAB_CLIENT_ID / SCHWAB_CLIENT_SECRET (or --client-id / "
            "--client-secret)."
        )
    else:
        lines += [
            "The refresh token has expired or been revoked. Schwab refresh "
            "tokens live 7 days from the last interactive login and cannot "
            "be renewed programmatically.",
            "Fix: mint a new one with login.py:",
            f"    login.py --token-path {token_path}",
        ]
    return "\n".join(lines)
