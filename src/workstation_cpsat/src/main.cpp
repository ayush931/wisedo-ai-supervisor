#include "solver.h"

#include <cstdlib>
#include <cmath>
#include <iostream>
#include <iterator>
#include <stdexcept>
#include <string>

int main(int argc, char** argv) {
  double seconds = 5.0;
  int seed = 1, workers = 1;
  try {
    for (int i = 1; i < argc; ++i) {
      const std::string arg = argv[i];
      if (i + 1 >= argc) throw std::invalid_argument("missing trusted solver argument");
      const std::string value = argv[++i];
      if (arg == "--time-limit-seconds") seconds = std::stod(value);
      else if (arg == "--seed") seed = std::stoi(value);
      else if (arg == "--workers") workers = std::stoi(value);
      else throw std::invalid_argument("unknown solver argument");
    }
    if (!std::isfinite(seconds) || seconds <= 0 || seconds > 60 || workers < 1 || workers > 32) throw std::invalid_argument("solver arguments outside bounds");
    std::string input((std::istreambuf_iterator<char>(std::cin)), std::istreambuf_iterator<char>());
    if (input.empty() || input.size() > 1024 * 1024) throw std::invalid_argument("input missing or exceeds 1 MiB");
    auto request = workstation_cpsat::ParseJsonStrict(input);
    std::cout << workstation_cpsat::Solve(request, seconds, seed, workers).dump() << '\n';
  } catch (const std::exception& error) {
    std::cerr << "invalid scheduling request: " << error.what() << '\n';
    return 2;
  }
  return 0;
}
