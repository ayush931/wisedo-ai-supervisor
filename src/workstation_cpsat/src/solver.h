#pragma once

#include <nlohmann/json.hpp>

#include <string>

namespace workstation_cpsat {
nlohmann::json Solve(const nlohmann::json& request, double time_limit_seconds,
                     int random_seed, int workers);
nlohmann::json ParseJsonStrict(const std::string& input);
}
