#include "solver.h"

#include <cassert>
#include <string>

using nlohmann::json;

json request() {
  return json{
      {"schema_version", 1}, {"line_id", "line-1"}, {"starts_at", "2026-10-20T06:00:00-04:00"},
      {"ends_at", "2026-10-20T14:00:00-04:00"}, {"timezone", "America/Detroit"},
      {"orders", json::array({{{"job_order_id", "order-1"}, {"job_order_revision", 1}, {"priority", 1},
        {"operations", json::array({{{"operation_id", "op-1"}, {"sequence", 1}, {"workstation_id", "station-1"}, {"name", "cut"}, {"quantity", 3600}, {"seconds_per_unit", "1"}, {"duration_basis", "standard_cycle"}},
                                     {{"operation_id", "op-2"}, {"sequence", 2}, {"workstation_id", "station-2"}, {"name", "pack"}, {"quantity", 1800}, {"seconds_per_unit", "1"}, {"duration_basis", "standard_cycle"}}})}}})},
      {"workstations", json::array({{{"id", "station-1"}, {"capacity", 1}}, {{"id", "station-2"}, {"capacity", 1}}})},
      {"downtimes", json::array({{{"workstation_id", "station-1"}, {"starts_at", "2026-10-20T06:00:00-04:00"}, {"ends_at", "2026-10-20T07:00:00-04:00"}, {"demand", 1}}})}};
}

int main() {
  const auto siblings = workstation_cpsat::ParseJsonStrict(R"([{"x":1},{"x":2}])");
  assert(siblings.size() == 2);
  bool duplicate_rejected = false;
  try { workstation_cpsat::ParseJsonStrict(R"({"a":1,"a":2})"); } catch (...) { duplicate_rejected = true; }
  assert(duplicate_rejected);
  bool nested_duplicate_rejected = false;
  try { workstation_cpsat::ParseJsonStrict(R"({"outer":{"x":1,"x":2}})"); } catch (...) { nested_duplicate_rejected = true; }
  assert(nested_duplicate_rejected);
  const auto result = workstation_cpsat::Solve(request(), 5, 7, 1);
  assert(result.at("status") == "OPTIMAL");
  assert(result.at("is_optimized") == true);
  assert(result.at("makespan_seconds") == 9000);
  const auto& tasks = result.at("assignments");
  assert(tasks.size() == 2);
  assert(tasks[0].at("starts_at") == "2026-10-20T11:00:00Z");
  assert(tasks[1].at("starts_at") >= tasks[0].at("ends_at"));

  auto impossible = request();
  impossible["ends_at"] = "2026-10-20T07:30:00-04:00";
  const auto infeasible = workstation_cpsat::Solve(impossible, 5, 7, 1);
  assert(infeasible.at("status") == "INFEASIBLE");
  assert(infeasible.at("assignments").empty());

  auto invalid = request();
  invalid["unexpected"] = true;
  bool rejected = false;
  try { workstation_cpsat::Solve(invalid, 5, 7, 1); } catch (...) { rejected = true; }
  assert(rejected);

  auto fractional = request();
  fractional["orders"][0]["operations"][0]["quantity"] = 2;
  fractional["orders"][0]["operations"][0]["seconds_per_unit"] = "0.000001";
  fractional["orders"][0]["operations"][1]["quantity"] = 1;
  fractional["orders"][0]["operations"][1]["seconds_per_unit"] = "0.000001";
  fractional["downtimes"] = json::array();
  const auto rounded = workstation_cpsat::Solve(fractional, 5, 7, 1);
  assert(rounded.at("status") == "OPTIMAL");
  assert(rounded.at("assignments")[0].at("duration_seconds") == 1);

  bool bad_date_rejected = false;
  try { auto malformed = request(); malformed["starts_at"] = "2026-02-30T06:00:00Z"; workstation_cpsat::Solve(malformed, 5, 7, 1); } catch (...) { bad_date_rejected = true; }
  assert(bad_date_rejected);
}
