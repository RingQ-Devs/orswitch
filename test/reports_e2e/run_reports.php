<?php
// Runs Core's report classes on the sandbox database. Usage: php runner.php '<json spec>'
//   spec: {"domain": "<domain_uuid>", "tz": "UTC", "start": 0, "end": 0, "reports": ["team_queue", ...], "fqdn": "..."}
// Prints one JSON object: {"<report>": <result or {"error": "..."}>}.
require_once APP_PATH . "/library/HubReports.php";
$spec = json_decode($argv[1] ?? '{}', true) ?: [];
$domain = $spec['domain'] ?? '';
$tz = $spec['tz'] ?? 'UTC';
$start = intval($spec['start'] ?? 0);
$end = intval($spec['end'] ?? 0);
$qext = $spec['queue'] ?? null;
$hub = new HubReports();
$hub->setDI($di);
$gen = new ReportGenerator();
$gen->setDI($di);
$out = [];
$run = function ($name, $fn) use (&$out, $spec) {
    if (!empty($spec['reports']) && !in_array($name, $spec['reports'], true)) {
        return;
    }
    try {
        $out[$name] = $fn();
    } catch (\Throwable $e) {
        $out[$name] = ["error" => get_class($e) . ": " . $e->getMessage()];
    }
};
$f = ["start" => $start, "end" => $end, "group" => "day", "sentiment" => "", "extension" => "", "queue" => "", "search" => "",
      "trunk" => "", "tariff" => "", "direction" => "", "scope" => "", "queuecalls" => "", "fqdn" => $spec['fqdn'] ?? ""];
foreach (array_keys(HubReports::REPORTS) as $key) {
    $run("hub:" . $key, function () use ($hub, $key, $domain, $tz, $f) { return $hub->report($key, $domain, $tz, $f); });
}
$run("overall", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->overallQueueCalls($domain, $tz, $qext, $start, $end); });
$run("overall_summary", function () use ($gen, $domain, $qext, $start, $end) { return $gen->overallQueueSummary($domain, $qext, $start, $end); });
$run("detail_queue", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->detailQueueStats($domain, $tz, $qext, $start, $end); });
$run("sla", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->slaStatistics($domain, $tz, $qext, $start, $end, ["target" => 0, "ignore" => 0, "after" => 0]); });
$run("sla_breaches", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->slaBreaches($domain, $tz, $qext, $start, $end, ["target" => 5, "ignore" => 0, "after" => 0]); });
$run("wait_time", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->answeredByWaitingTime($domain, $tz, $qext, $start, $end, ["target" => 0, "ignore" => 0, "after" => 0]); });
$run("abandoned", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->abandonedQueueCalls($domain, $tz, $qext, $start, $end, ["target" => 0, "ignore" => 0, "after" => 0]); });
$run("queue_recovery", function () use ($gen, $domain, $tz, $qext, $start, $end) { return $gen->queueRecoveryReport($domain, $tz, $qext, $start, $end); });
echo json_encode($out, JSON_PARTIAL_OUTPUT_ON_ERROR | JSON_INVALID_UTF8_SUBSTITUTE), "\n";
