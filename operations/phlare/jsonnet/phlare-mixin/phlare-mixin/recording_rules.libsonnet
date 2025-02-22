local utils = import 'mixin-utils/utils.libsonnet';

{
  prometheusRules+:: {
    groups+: [{
      name: 'phlare_rules',
      local cluster = if $._config.multi_cluster then [$._config.per_cluster_label] else [],
      rules:
        utils.histogramRules('phlare_request_duration_seconds', ['job'] + cluster) +
        utils.histogramRules('phlare_request_duration_seconds', ['job', 'route'] + cluster) +
        utils.histogramRules('phlare_request_duration_seconds', ['namespace', 'job', 'route'] + cluster) +
        utils.histogramRules('phlare_distributor_received_compressed_bytes', ['job', 'type'] + cluster) +
        utils.histogramRules('phlare_distributor_received_samples', ['job', 'type'] + cluster),
    }],
  },
}
