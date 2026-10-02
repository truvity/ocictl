// Package smserver serves source maps stored as OCI artifacts (see
// pkg/sourcemaps) over the URL shape Grafana Alloy's faro.receiver asks for:
//
//	GET|HEAD /<app>/<release>/<script path>.map
//
// <app> must be a configured application name; it selects a repository from
// the configuration and is never used to build a registry host, so a
// request cannot make the server talk to a registry of its choosing.
// <release> is the Faro app release; after the same "+" to "_" sanitising
// the publisher applies it is the OCI tag.
//
// On first use of an (app, release) the server pulls the artifact, checks it
// (artifact type, one tar.gz layer, size caps, blob digest), unpacks it into
// a size-capped LRU directory with os.Root and serves the files from there.
package smserver
