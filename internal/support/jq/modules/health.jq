# health — built-in jq module with snowplow's normalized health/usage
# semantics. Generic mechanism (no product/service opinion): any
# RESTAction filter or widget template can `include "health";` and
# aggregate heterogeneous per-service health signals into the single
# normalized vocabulary OK / Warning / Critical / Unknown.
#
# Severity order (aggregation): Critical > Warning > Unknown > OK.
# An unreadable/absent signal degrades the rollup (Unknown), but never
# below an explicit Warning/Critical.

# normalize_health — map an arbitrary raw health value (string, bool,
# number, null) onto OK / Warning / Critical / Unknown.
#
# Numbers are interpreted as this module's own severity codes — the exact
# inverse of health_severity below, so a value round-trips:
# (health_severity | normalize_health) == normalize_health.
#
#   0 = OK, 1 = Unknown, 2 = Warning, 3 = Critical
#
# Any other number is Unknown: an unmapped signal must degrade the
# rollup, never silently pass as healthy. Note this is NOT the
# Nagios/Icinga plugin exit-code convention (0/1/2/3 =
# OK/Warning/Critical/Unknown there) — a Nagios-shaped source should
# translate its codes before aggregation.
def normalize_health:
  if type == "number" then
    (if . == 0 then "OK"
     elif . == 1 then "Unknown"
     elif . == 2 then "Warning"
     elif . == 3 then "Critical"
     else "Unknown"
     end)
  else
    (if . == null then ""
     elif type == "boolean" then (if . then "true" else "false" end)
     else (tostring | ascii_downcase)
     end) as $s
    | if $s == "" then "Unknown"
      elif (["ok","healthy","health_ok","ready","running","up","green",
             "active","available","succeeded","success","passing","normal",
             "online","bound","synced","true"] | index($s)) then "OK"
      elif (["warning","warn","degraded","yellow","progressing","pending",
             "provisioning","updating","scaling","suspended","partial",
             "minor","paused"] | index($s)) then "Warning"
      elif (["critical","crit","error","failed","failure","red","down",
             "unavailable","offline","lost","crashloopbackoff","outofsync",
             "notready","false","major","fatal","unhealthy"] | index($s)) then "Critical"
      else "Unknown"
      end
  end;

# health_severity — numeric severity of a raw health value (for sorting).
def health_severity:
  {"Critical": 3, "Warning": 2, "Unknown": 1, "OK": 0}[normalize_health];

# worst_health — input: array of raw health values; output: the
# normalized worst status ("Unknown" for an empty array).
def worst_health:
  map(normalize_health) as $n
  | if ($n | length) == 0 then "Unknown"
    elif ($n | index("Critical")) then "Critical"
    elif ($n | index("Warning")) then "Warning"
    elif ($n | index("Unknown")) then "Unknown"
    else "OK"
    end;

# health_summary(f) — input: array of objects; f extracts the raw health
# value from each. Output: counts per normalized status + overall.
def health_summary(f):
  map(f | normalize_health) as $n
  | { total:    ($n | length),
      ok:       ([$n[] | select(. == "OK")]       | length),
      warning:  ([$n[] | select(. == "Warning")]  | length),
      critical: ([$n[] | select(. == "Critical")] | length),
      unknown:  ([$n[] | select(. == "Unknown")]  | length),
      overall:  ($n | worst_health) };

# health_summary — shorthand over the conventional `.health` field.
def health_summary: health_summary(.health);

# health_rollup(g; f) — group an array of objects by key expression g
# (e.g. .org, .tenant, .service) and emit one summary per group. This is
# the consolidated-dashboard shape: filterable by whatever dimension g
# extracts.
def health_rollup(g; f):
  group_by(g)
  | map({ key: (.[0] | g) } + health_summary(f));

# usage_pct(used; capacity) — percentage (0-100, 1 decimal) or null when
# capacity is null/0 (unknown capacity must not fake a 0% usage).
def usage_pct(used; capacity):
  (capacity) as $c
  | if ($c == null or $c == 0) then null
    else (((used / $c) * 1000 | round) / 10)
    end;

# usage_health(pct; warnAt; critAt) — derive a normalized status from a
# usage percentage against two thresholds.
def usage_health(pct; warnAt; critAt):
  (pct) as $p
  | if $p == null then "Unknown"
    elif $p >= critAt then "Critical"
    elif $p >= warnAt then "Warning"
    else "OK"
    end;

# usage_summary(fu; fc; warnAt; critAt) — input: array of objects; fu/fc
# extract used/capacity. Output: totals + percentage + derived status.
def usage_summary(fu; fc; warnAt; critAt):
  ([.[] | (fu // 0)] | add // 0) as $used
  | (if ([.[] | fc] | all(. != null)) then ([.[] | fc] | add) else null end) as $cap
  | usage_pct($used; $cap) as $pct
  | { used: $used,
      capacity: $cap,
      pct: $pct,
      status: usage_health($pct; warnAt; critAt) };
