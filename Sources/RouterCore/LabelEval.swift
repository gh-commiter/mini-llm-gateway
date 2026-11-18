import Foundation

extension Router {
    /// POST /usage/labels/eval (the body: `{"examples":[{"text","phase","activity"}]}`, at most 500): the shape is checked here, the labels and the
    /// run by `LabelWorker.evaluate`. The texts are held in memory for the run and never logged or answered (see LabelWorker).
    func labelsEval(_ body: Data) async throws -> (Int, Any) {
        let shape = AdminError(400, "examples: a list of {text, phase, activity}")
        guard let labels else { throw AdminError(503, "The project database is not available: the project helper (minigw-core) is not running") }
        guard let o = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any], o.keys.allSatisfy({ $0.lowercased() == "examples" }),
              let list = o.first?.value as? [Any] else { throw shape }
        guard (1...LabelWorker.evalMaxExamples).contains(list.count) else { throw AdminError(400, "examples: 1 to \(LabelWorker.evalMaxExamples)") }
        var examples: [LabelWorker.EvalExample] = []
        for item in list {
            guard let d = item as? [String: Any] else { throw shape }
            var f: [String: String] = [:]
            for (k, v) in d {
                guard ["text", "phase", "activity"].contains(k.lowercased()) else { throw shape }
                if v is NSNull { continue }
                guard let s = v as? String else { throw shape }
                f[k.lowercased()] = s
            }
            examples.append(.init(text: f["text"] ?? "", phase: f["phase"] ?? "", activity: f["activity"] ?? ""))
        }
        let (status, data) = await labels.evaluate(examples)
        return (status, (try? JSONSerialization.jsonObject(with: data)) ?? [:])
    }
}
