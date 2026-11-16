# Security policy

## Reporting a vulnerability

Please do not open a public issue, pull request or discussion for a suspected
vulnerability. Report it privately by e-mail to `hello@rec4ll.com` with the
subject line `SECURITY`.

Include what you can of:

- the release or commit you tested and the affected component (the miner,
  `db-service`, `model-proxy`, the inference or Chutes sidecar, a Compose file or
  a setup guide);
- what an attacker needs, what they gain, and the steps that show it;
- whether the issue is already known to anyone else.

Do not include real credentials, wallet secrets or customer data in a report.

## What to expect

- We aim to acknowledge a report within 3 working days and to give a first
  assessment within 10 working days.
- We will keep you informed while a fix is prepared and agree a disclosure date
  with you. Please give us reasonable time to ship a fix before publishing.

## Supported versions

Only the latest release receives security fixes.

## Scope

In scope: the code, Compose files and setup guides in this repository. Examples:
anything that lets a party other than the Recall API make this miner do work,
read or change stored data, or obtain its secrets; crashes or resource
exhaustion reachable from the public port; container, file-permission or Compose
defaults that weaken an operator who follows `SETUP.md`.

Out of scope, and not permitted:

- attacks on the hosted Recall API, or on miners or validators you do not operate;
- problems that need root on the miner host or an already compromised host;
- social engineering of operators.

Issues in the Recall API or the validator belong in those repositories; use the
same process there. Vulnerabilities in third-party images or dependencies go
upstream; tell us if a pinned version here needs to move.
