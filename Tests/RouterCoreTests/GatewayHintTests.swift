import Foundation
import Testing
@testable import RouterCore

@Suite struct GatewayHintParity {
    /// client/testdata/gateway-hint.json is asserted by the Go client too (profile_hint_test.go).
    @Test func sharedCases() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let f = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/gateway-hint.json"))) as! [String: Any]
        for c in f["cases"] as! [[String: Any]] {
            let got = ProjectError.hint(c["message"] as! String, status: c["status"] as! Int, profile: c["profile"] as! String, url: c["url"] as! String, profiles: c["profiles"] as! Int)
            #expect(got == c["out"] as! String, "\(c["name"] as! String)")
        }
    }

    @Test func aGatewayAnswerCarriesItsStatus() {
        #expect(ProjectEnv.gatewayError(404, Data(), nil).gatewayStatus == 404)
        #expect(ProjectEnv.gatewayError(403, Data(), nil).gatewayStatus == 403)
        #expect(ProjectError("local problem").gatewayStatus == 0)
    }
}
