#include "solver.h"

#include <ortools/sat/cp_model.h>
#include <ortools/sat/cp_model_solver.h>

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <ctime>
#include <iomanip>
#include <limits>
#include <map>
#include <regex>
#include <set>
#include <sstream>
#include <stdexcept>
#include <string>
#include <tuple>
#include <vector>

namespace workstation_cpsat {
nlohmann::json ParseJsonStrict(const std::string& input) {
  bool duplicate_key = false;
  std::map<int, std::set<std::string>> object_keys;
  const auto callback = [&duplicate_key, &object_keys](int depth, nlohmann::json::parse_event_t event, nlohmann::json& parsed) {
    if (event == nlohmann::json::parse_event_t::object_start) object_keys[depth].clear();
    else if (event == nlohmann::json::parse_event_t::key && !object_keys[depth - 1].insert(parsed.get<std::string>()).second) duplicate_key = true;
    else if (event == nlohmann::json::parse_event_t::object_end) object_keys.erase(depth);
    return true;
  };
  auto parsed = nlohmann::json::parse(input, callback);
  if (duplicate_key) throw std::invalid_argument("duplicate JSON object key");
  return parsed;
}

namespace {
using operations_research::Domain;
using operations_research::sat::CpModelBuilder;
using operations_research::sat::CpSolverResponse;
using operations_research::sat::IntVar;
using operations_research::sat::IntervalVar;
using operations_research::sat::SatParameters;
using operations_research::sat::SolveCpModel;
using operations_research::sat::SolutionIntegerValue;
using operations_research::sat::CpSolverStatus;
using operations_research::sat::Model;
using operations_research::sat::NewSatParameters;

constexpr int64_t kMaxHorizon = 24 * 60 * 60;
constexpr size_t kMaxOrders = 50, kMaxOperations = 200, kMaxStations = 100, kMaxDowntimes = 100;

int64_t ParseTime(const std::string& value) {
  static const std::regex pattern(R"(^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(Z|[+-][0-9]{2}:[0-9]{2})$)");
  std::smatch match;
  if (!std::regex_match(value, match, pattern)) throw std::invalid_argument("timestamp must be RFC3339 with offset");
  int year = std::stoi(match[1]), month = std::stoi(match[2]), day = std::stoi(match[3]);
  int hour = std::stoi(match[4]), minute = std::stoi(match[5]), second = std::stoi(match[6]);
  if (match[7].matched && match[7].str().find_first_not_of('0') != std::string::npos) throw std::invalid_argument("subsecond timestamps are unsupported by the seconds-based solver");
  int offset = 0;
  const auto zone = match[8].str();
  if (zone != "Z") {
    const int oh = std::stoi(zone.substr(1, 2)), om = std::stoi(zone.substr(4, 2));
    if (oh > 14 || om > 59 || (oh == 14 && om != 0)) throw std::invalid_argument("invalid RFC3339 offset");
    offset = (oh * 60 + om) * (zone[0] == '+' ? 1 : -1);
  }
  if (year < 1 || month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59) throw std::invalid_argument("invalid RFC3339 timestamp");
  std::tm tm{};
  tm.tm_year = year - 1900; tm.tm_mon = month - 1; tm.tm_mday = day;
  tm.tm_hour = hour; tm.tm_min = minute; tm.tm_sec = second;
  const auto local = timegm(&tm);
  std::tm check{};
  gmtime_r(&local, &check);
  if (check.tm_year != year - 1900 || check.tm_mon != month - 1 || check.tm_mday != day) throw std::invalid_argument("invalid calendar date");
  return static_cast<int64_t>(local) - offset * 60;
}

std::string FormatTime(int64_t epoch) {
  std::time_t value = static_cast<std::time_t>(epoch);
  std::tm tm{};
  gmtime_r(&value, &tm);
  std::ostringstream out;
  out << std::put_time(&tm, "%Y-%m-%dT%H:%M:%SZ");
  return out.str();
}

void ExactKeys(const nlohmann::json& value, std::initializer_list<const char*> keys) {
  if (!value.is_object()) throw std::invalid_argument("expected object");
  std::set<std::string> allowed;
  for (const auto* key : keys) allowed.emplace(key);
  for (const auto& item : value.items()) {
    if (!allowed.count(item.key())) throw std::invalid_argument("unknown field: " + item.key());
  }
  for (const auto& key : allowed) {
    if (!value.contains(key) && key != "ready_at" && key != "due_at") throw std::invalid_argument("missing field: " + key);
  }
}

int64_t Duration(const nlohmann::json& op) {
  const auto rate_text = op.at("seconds_per_unit").get<std::string>();
  static const std::regex decimal(R"(^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$)");
  std::smatch match;
  if (!std::regex_match(rate_text, match, decimal)) throw std::invalid_argument("seconds_per_unit must be a canonical decimal with at most six places");
  const int64_t whole = std::stoll(match[1].str());
  if (whole > kMaxHorizon) throw std::invalid_argument("operation exceeds shift horizon");
  std::string fraction = match[2].matched ? match[2].str().substr(1) : "";
  fraction.resize(6, '0');
  const int64_t rate_micros = whole * 1000000 + (fraction.empty() ? 0 : std::stoll(fraction));
  const int64_t quantity = op.at("quantity").get<int64_t>();
  if (rate_micros <= 0 || quantity < 1 || rate_micros > (kMaxHorizon * 1000000) / quantity) throw std::invalid_argument("operation exceeds shift horizon");
  const int64_t product = rate_micros * quantity;
  return (product + 999999) / 1000000;
}

struct Task {
  std::string order, operation, station, basis;
  int sequence;
  int64_t duration, ready;
  IntVar start, end;
  IntervalVar interval;
};
}

nlohmann::json Solve(const nlohmann::json& request, double time_limit_seconds, int random_seed, int workers) {
  ExactKeys(request, {"schema_version", "line_id", "starts_at", "ends_at", "timezone", "orders", "workstations", "downtimes"});
  if (request.at("schema_version") != 1 || !request.at("line_id").is_string() || request.at("line_id").get<std::string>().empty() || !request.at("timezone").is_string() || request.at("timezone").get<std::string>().empty()) throw std::invalid_argument("unsupported request");
  const auto anchor = ParseTime(request.at("starts_at").get<std::string>());
  const auto shift_end = ParseTime(request.at("ends_at").get<std::string>());
  const int64_t horizon = shift_end - anchor;
  if (horizon < 1 || horizon > kMaxHorizon || !request.at("orders").is_array() || request.at("orders").empty() || request.at("orders").size() > kMaxOrders || !request.at("workstations").is_array() || request.at("workstations").empty() || request.at("workstations").size() > kMaxStations || !request.at("downtimes").is_array() || request.at("downtimes").size() > kMaxDowntimes) throw std::invalid_argument("request outside supported bounds");

  std::map<std::string, int> capacities;
  for (const auto& station : request.at("workstations")) {
    ExactKeys(station, {"id", "capacity"});
    const auto id = station.at("id").get<std::string>();
    const int cap = station.at("capacity").get<int>();
    if (id.empty() || cap < 1 || cap > 64 || !capacities.emplace(id, cap).second) throw std::invalid_argument("invalid workstation");
  }

  CpModelBuilder model;
  std::map<std::string, std::vector<IntervalVar>> intervals;
  std::map<std::string, std::vector<int64_t>> demands;
  std::vector<Task> tasks;
  std::set<std::string> order_ids;
  for (const auto& order : request.at("orders")) {
    ExactKeys(order, {"job_order_id", "job_order_revision", "ready_at", "due_at", "priority", "operations"});
    const auto order_id = order.at("job_order_id").get<std::string>();
    if (order_id.empty() || !order_ids.insert(order_id).second || order.at("job_order_revision").get<int64_t>() < 1 || order.at("priority").get<int>() < 0 || order.at("priority").get<int>() > 1000 || !order.at("operations").is_array() || order.at("operations").empty()) throw std::invalid_argument("invalid job order");
    int64_t ready = 0;
    if (order.contains("ready_at") && !order.at("ready_at").is_null()) ready = std::max<int64_t>(0, ParseTime(order.at("ready_at").get<std::string>()) - anchor);
    if (ready > horizon) throw std::invalid_argument("ready time outside shift");
    if (order.contains("due_at") && !order.at("due_at").is_null()) ParseTime(order.at("due_at").get<std::string>());
    if (order.at("operations").size() + tasks.size() > kMaxOperations) throw std::invalid_argument("too many operations");
    std::set<std::string> op_ids;
    int previous_sequence = 0;
    int previous_task = -1;
    for (const auto& op : order.at("operations")) {
      ExactKeys(op, {"operation_id", "sequence", "workstation_id", "name", "quantity", "seconds_per_unit", "duration_basis"});
      const auto op_id = op.at("operation_id").get<std::string>();
      const auto station = op.at("workstation_id").get<std::string>();
      const int sequence = op.at("sequence").get<int>();
      if (op_id.empty() || !op_ids.insert(op_id).second || sequence <= previous_sequence || !capacities.count(station)) throw std::invalid_argument("invalid operation route");
      const int64_t duration = Duration(op);
      const auto start = model.NewIntVar(Domain(0, horizon - duration)).WithName("s" + std::to_string(tasks.size()));
      const auto end = model.NewIntVar(Domain(duration, horizon)).WithName("e" + std::to_string(tasks.size()));
      const auto interval = model.NewIntervalVar(start, duration, end).WithName("i" + std::to_string(tasks.size()));
      if (previous_task >= 0) model.AddGreaterOrEqual(start, tasks[previous_task].end);
      else if (ready > 0) model.AddGreaterOrEqual(start, ready);
      intervals[station].push_back(interval); demands[station].push_back(1);
      tasks.push_back({order_id, op_id, station, op.at("duration_basis").get<std::string>(), sequence, duration, ready, start, end, interval});
      previous_task = static_cast<int>(tasks.size()) - 1;
      previous_sequence = sequence;
    }
  }
  for (const auto& down : request.at("downtimes")) {
    ExactKeys(down, {"workstation_id", "starts_at", "ends_at", "demand"});
    const auto station = down.at("workstation_id").get<std::string>();
    if (!capacities.count(station)) throw std::invalid_argument("unknown downtime workstation");
    int64_t start = std::clamp<int64_t>(ParseTime(down.at("starts_at").get<std::string>()) - anchor, 0, horizon);
    int64_t end = std::clamp<int64_t>(ParseTime(down.at("ends_at").get<std::string>()) - anchor, 0, horizon);
    const int demand = down.at("demand").get<int>();
    if (end < start || demand < 1 || demand > capacities.at(station)) throw std::invalid_argument("invalid downtime");
    if (end == start) continue;
    auto interval = model.NewIntervalVar(start, end - start, end).WithName("downtime");
    intervals[station].push_back(interval); demands[station].push_back(demand);
  }
  for (const auto& station : capacities) {
    auto cumulative = model.AddCumulative(station.second);
    for (size_t i = 0; i < intervals[station.first].size(); ++i) cumulative.AddDemand(intervals[station.first][i], demands[station.first][i]);
  }
  const auto makespan = model.NewIntVar(Domain(0, horizon)).WithName("makespan");
  std::vector<IntVar> ends;
  for (const auto& task : tasks) ends.push_back(task.end);
  model.AddMaxEquality(makespan, ends);
  model.Minimize(makespan);
  SatParameters parameters;
  parameters.set_max_time_in_seconds(time_limit_seconds);
  parameters.set_random_seed(random_seed);
  parameters.set_num_search_workers(workers);
  Model solver_model;
  solver_model.Add(NewSatParameters(parameters));
  CpSolverResponse result = SolveCpModel(model.Build(), &solver_model);

  std::string status;
  if (result.status() == CpSolverStatus::OPTIMAL) status = "OPTIMAL";
  else if (result.status() == CpSolverStatus::FEASIBLE) status = "FEASIBLE";
  else if (result.status() == CpSolverStatus::INFEASIBLE) status = "INFEASIBLE";
  else status = "UNKNOWN";
  nlohmann::json response{{"schema_version", 1}, {"method", "ortools_cp_sat"}, {"status", status}, {"is_optimized", status == "OPTIMAL"}, {"assignments", nlohmann::json::array()}};
  if (status == "FEASIBLE" || status == "OPTIMAL") {
    response["makespan_seconds"] = SolutionIntegerValue(result, makespan);
    for (const auto& task : tasks) {
      const auto start = anchor + SolutionIntegerValue(result, task.start);
      const auto end = anchor + SolutionIntegerValue(result, task.end);
      response["assignments"].push_back({{"job_order_id", task.order}, {"operation_id", task.operation}, {"sequence", task.sequence}, {"workstation_id", task.station}, {"starts_at", FormatTime(start)}, {"ends_at", FormatTime(end)}, {"duration_seconds", task.duration}, {"duration_basis", task.basis}});
    }
    std::sort(response["assignments"].begin(), response["assignments"].end(), [](const nlohmann::json& a, const nlohmann::json& b) {
      return std::make_tuple(a["starts_at"].get<std::string>(), a["job_order_id"].get<std::string>(), a["sequence"].get<int>()) <
             std::make_tuple(b["starts_at"].get<std::string>(), b["job_order_id"].get<std::string>(), b["sequence"].get<int>());
    });
  }
  return response;
}
}
