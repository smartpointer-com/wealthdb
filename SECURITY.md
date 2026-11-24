# Security

wealthdb signs in to banks and brokers with credentials that can move
money, and it keeps a household's financial data. A security problem
here is taken seriously.

The [README](README.md) says what the software promises about
credentials and data, and what it does not.

## Reporting a problem

Report a security problem in private, not in a public issue. Use
**Report a vulnerability** under the **Security** tab of this
repository. A report says what was found and how to see it.

For a problem in the Homebrew formula, use the same button in the
[tap's repository](https://github.com/smartpointer-com/homebrew-tap/security).

## What counts

- A collector that can change something at a bank or broker: an order,
  a transfer, a setting.
- A credential, session or token that reaches a log, a file other
  accounts can read, or another computer.
- Financial data that leaves the computer in a way the README does not
  describe.
- A way to reach the dashboards or the MCP server without their sign-in
  or token, or to make the MCP server change data.
- A build or install step that runs code the source does not pin.

## Which versions get a fix

A fix goes into the next release. A published release is never changed.
