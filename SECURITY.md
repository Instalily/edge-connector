# Security policy

The edge connector runs inside customers' networks, so we treat security
reports as our highest priority.

## Reporting a vulnerability

Report it privately: on this repository's **Security** tab, choose **Report a
vulnerability**. Please don't open a public issue or pull request for it.

Include what you can of:

- the component: connector, hub, `edgectl`, the Helm chart, or a deployment
  template;
- the version, or the image digest;
- how to reproduce it, and what an attacker gains.

We'll acknowledge the report, keep you updated while we fix it, and credit you
in the release notes if you want.

## What counts

The properties in the README's [security model](README.md#security-model) are
the ones we defend. For example:

- reaching a target that isn't on the connector's allowlist;
- the hub, or anyone else, opening a connection through a connector it
  shouldn't, or a connector reaching Instalily's side;
- connecting to a hub, enrolling or renewing without valid credentials;
- a key or certificate leaving the place it belongs.

## Supported versions

Fixes ship in a new release. Run the latest one: releases are listed on the
[releases page](https://github.com/Instalily/edge-connector/releases), and the
README explains how to verify an image's signature.
