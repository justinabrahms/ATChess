#!/bin/bash

# Run all bug regression tests
# This script runs tests for all discovered bugs to ensure they remain fixed

set -e

echo "🐛 ATChess Bug Regression Tests"
echo "==============================="
echo ""

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to run a specific test
run_test() {
    local test_name=$1
    local test_function=$2
    
    echo -n "Running $test_name... "
    
    if go test -v ./test/bugs -run "$test_function" > /tmp/test_output 2>&1; then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC}"
        echo "Test output:"
        cat /tmp/test_output
        echo ""
        return 1
    fi
}

# Ensure we're in the project root
cd "$(dirname "$0")/.."

# Check if Go is available
if ! command -v go &> /dev/null; then
    echo -e "${RED}Error: Go is not installed or not in PATH${NC}"
    exit 1
fi

# Initialize Go module if needed
if [ ! -f go.mod ]; then
    echo -e "${YELLOW}Initializing Go module...${NC}"
    go mod init github.com/justinabrahms/atchess
    go mod tidy
fi

echo "🧪 Running individual bug tests:"
echo ""

# Test each bug individually
test_results=()

# Bug 1: CORS Options Request Handling
if run_test "Bug 1: CORS Options Request Handling" "TestBug1_CORSOptionsRequestHandling"; then
    test_results+=("✅ Bug 1: CORS Options Request Handling")
else
    test_results+=("❌ Bug 1: CORS Options Request Handling")
fi

# Bug 2: AT Protocol URI Routing
if run_test "Bug 2: AT Protocol URI Routing" "TestBug2_ATProtocolURIRouting"; then
    test_results+=("✅ Bug 2: AT Protocol URI Routing")
else
    test_results+=("❌ Bug 2: AT Protocol URI Routing")
fi

# Bug 3: Missing JSON Struct Tags
if run_test "Bug 3: Missing JSON Struct Tags" "TestBug3_MissingJSONStructTags"; then
    test_results+=("✅ Bug 3: Missing JSON Struct Tags")
else
    test_results+=("❌ Bug 3: Missing JSON Struct Tags")
fi

# Bug 4: Empty FEN String Validation
if run_test "Bug 4: Empty FEN String Validation" "TestBug4_EmptyFENStringValidation"; then
    test_results+=("✅ Bug 4: Empty FEN String Validation")
else
    test_results+=("❌ Bug 4: Empty FEN String Validation")
fi

# Bug 5: AT Protocol URI Parsing
if run_test "Bug 5: AT Protocol URI Parsing" "TestBug5_ATProtocolURIParsing"; then
    test_results+=("✅ Bug 5: AT Protocol URI Parsing")
else
    test_results+=("❌ Bug 5: AT Protocol URI Parsing")
fi

# Bug 6: Base64 Padding Truncation
if run_test "Bug 6: Base64 Padding Truncation" "TestBug6_Base64PaddingTruncation"; then
    test_results+=("✅ Bug 6: Base64 Padding Truncation")
else
    test_results+=("❌ Bug 6: Base64 Padding Truncation")
fi

# Bug 7: Game Creation JSON Serialization
if run_test "Bug 7: Game Creation JSON Serialization" "TestBug7_GameCreationJSONSerialization"; then
    test_results+=("✅ Bug 7: Game Creation JSON Serialization")
else
    test_results+=("❌ Bug 7: Game Creation JSON Serialization")
fi

# Integration scenario
if run_test "Integration Scenario" "TestBug_IntegrationScenario"; then
    test_results+=("✅ Integration Scenario")
else
    test_results+=("❌ Integration Scenario")
fi

echo ""
echo "📊 Test Results Summary:"
echo "========================"
echo ""

# Count passed/failed tests
passed=0
failed=0

for result in "${test_results[@]}"; do
    echo "$result"
    if [[ $result == ✅* ]]; then
        ((passed++))
    else
        ((failed++))
    fi
done

echo ""
echo "Total tests: $((passed + failed))"
echo -e "${GREEN}Passed: $passed${NC}"
echo -e "${RED}Failed: $failed${NC}"

# Run all tests together for detailed output
echo ""
echo "🔄 Running all bug tests together:"
echo "=================================="

if go test -v ./test/bugs; then
    echo -e "${GREEN}All bug tests passed!${NC}"
    exit_code=0
else
    echo -e "${RED}Some bug tests failed!${NC}"
    exit_code=1
fi

echo ""
echo "💡 Tips:"
echo "- Run individual tests with: go test -v ./test/bugs -run TestBugN_"
echo "- Add -count=1 to disable test caching: go test -v -count=1 ./test/bugs"
echo "- Run with coverage: go test -v -cover ./test/bugs"
echo "- See test/bugs/discovered-bugs.md for detailed bug documentation"

exit $exit_code