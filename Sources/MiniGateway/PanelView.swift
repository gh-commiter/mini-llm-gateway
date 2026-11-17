import RouterCore
import SwiftUI

// The generic renderer of admin panels (docs/plans/plugin-panels-design.md). Thin on purpose: every decision (requests,
// placeholders, confirmations, states) is in RouterCore/PanelModel.swift, which has the tests. Every string of a description
// and every value of an answer is shown with Text(verbatim:), never as a LocalizedStringKey or Markdown, and nothing here
// opens a URL (PanelRendererSourceTests checks this file). Fixed strings of the renderer are string literals with an `sl`
// entry in Localizable.xcstrings.

struct PanelPane: View {
    let entry: PanelEntry
    let color: Color
    let send: PanelSend
    let strings: PanelStrings

    var body: some View {
        let header = PaneHeader(symbol: entry.sidebarSymbol.isEmpty ? "square.dashed" : entry.sidebarSymbol, color: color,
                                title: entry.title(strings), about: entry.about(strings))
        switch entry.parse {
        case .ready(let d):
            ReadyPanel(header: header, model: PanelModel(panel: d, strings: strings, routes: PanelRoutes.registered(for: entry.provider) ?? [], send: { [send] in await send($0) })).id(d.id)
        case .needsNewer:
            Form { header; Section { Text("This panel needs a newer Mini Gateway.").foregroundStyle(.secondary) } }
        case .refused(let why):
            Form {
                header
                Section {
                    Text("This panel cannot be shown.").foregroundStyle(.secondary)
                    Text(verbatim: why).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                }
            }
        }
    }
}

/// The sidebar page for a list of panels that could not be loaded at all.
struct PanelsUnavailablePane: View {
    let header: PaneHeader
    let error: PanelError
    var body: some View {
        Form { header; Section { PanelErrorText(error: error) } }
    }
}

private struct ReadyPanel: View {
    let header: PaneHeader
    @StateObject var model: PanelModel

    init(header: PaneHeader, model: PanelModel) { self.header = header; _model = StateObject(wrappedValue: model) }

    var body: some View {
        Form {
            header
            if let e = model.refError { Section { PanelErrorText(error: e) } }
            PanelSections(model: model, sections: model.panel.sections, path: "", parent: nil)
            Section {
                HStack {
                    if model.loading { ProgressView().controlSize(.small) }
                    Spacer()
                    Button("Refresh") { Task { await model.load() } }
                }
            }
        }
        .task { await model.load() }  // when the page is shown; never polled
    }
}

struct PanelErrorText: View {
    let error: PanelError
    var body: some View {
        Group {
            switch error {
            case .unavailable: Text("The project database is not available.")
            case .notFound: Text("The panels need a newer Mini Gateway: update the app.")
            case .needsNewer: Text("This panel needs a newer Mini Gateway.")
            case .server(let m): Text(verbatim: m)
            case .http(let n): Text(verbatim: "HTTP \(n)")
            case .serverFault(let n): Text("The gateway reported an error; the details are in its log.") + Text(verbatim: " (HTTP \(n))")
            case .cannotShow(let why):
                VStack(alignment: .leading, spacing: 2) {
                    Text("This part of the panel cannot be shown.")
                    Text(verbatim: why).font(.caption)
                }
            case .refused(let why):
                VStack(alignment: .leading, spacing: 2) {
                    Text("The request was not sent.")
                    Text(verbatim: why).font(.caption)
                }
            }
        }
        .foregroundStyle(.red).textSelection(.enabled)
    }
}

/// The sections of a page: `path` is "" at the top and "<list>/" inside the detail page of a list.
private struct PanelSections: View {
    @ObservedObject var model: PanelModel
    let sections: [PanelSection]
    let path: String
    let parent: PanelRow?

    var body: some View {
        ForEach(sections, id: \.id) { s in
            switch s.kind {
            case "text":
                Section { Text(verbatim: model.text(s.body)).foregroundStyle(.secondary) }
            case "list":
                PanelList(model: model, section: s, path: path + s.id, parent: parent)
            default:
                Section { Label { Text("This part of the panel cannot be shown.") } icon: { Image(systemName: "questionmark.square.dashed") }.foregroundStyle(.secondary) }
            }
        }
    }
}

private struct FormRequest: Identifiable { let id = UUID(); let action: PanelAction; let row: PanelRow? }
private struct ConfirmRequest: Identifiable { let id = UUID(); let action: PanelAction; let row: PanelRow?; let text: PanelConfirmText; let values: [String: PanelJSON] }

private struct PanelList: View {
    @ObservedObject var model: PanelModel
    let section: PanelSection
    let path: String
    let parent: PanelRow?
    @State private var form: FormRequest?
    @State private var confirming: ConfirmRequest?
    @State private var opened: PanelRow?

    private var key: PanelListKey { PanelListKey(path: path, parent: parent?.id ?? "") }

    var body: some View {
        let data = model.data(path, parent: parent)
        let rows = model.visibleRows(section, path: path, parent: parent)
        Section {
            if let e = data.error { PanelErrorText(error: e) }
            if let e = model.actionError[key] { PanelErrorText(error: e) }
            if let h = section.hide {
                Toggle(isOn: Binding(get: { data.showHidden }, set: { model.setShowHidden($0, path: path, parent: parent) })) { Text(verbatim: model.text(h.toggle)) }
            }
            if !(section.filter ?? []).isEmpty {
                TextField("Search", text: Binding(get: { data.search }, set: { model.setSearch($0, path: path, parent: parent) }))
            }
            if data.loading && data.rows.isEmpty { ProgressView().frame(maxWidth: .infinity) }
            ForEach(rows) { row in
                PanelRowView(model: model, section: section, row: row,
                             onAction: { act($0, row: row) }, onOpen: section.detail == nil ? nil : { opened = row })
            }
            if rows.isEmpty && !data.loading && data.error == nil, let e = section.empty { Text(verbatim: model.text(e)).foregroundStyle(.secondary) }
            if data.next != nil { Button("Load More") { Task { await model.loadMore(section, path: path, parent: parent) } } }
            if model.unsupportedCount(section) > 0 {
                Label { Text("This part of the panel cannot be shown.") } icon: { Image(systemName: "questionmark.square.dashed") }.foregroundStyle(.secondary)
            }
            let actions = model.actions(section)
            if !actions.isEmpty {
                EqualWidthHStack {
                    ForEach(actions, id: \.id) { a in
                        Button { act(a, row: nil) } label: { Text(verbatim: model.text(a.label)).frame(maxWidth: .infinity) }
                    }
                }
            }
        } header: {
            Text(verbatim: model.text(section.title))
        }
        .sheet(item: $form) { f in
            PanelFormSheet(model: model, action: f.action, row: f.row, parent: parent, path: path)
        }
        .sheet(item: $opened) { row in
            PanelDetailSheet(model: model, section: section, row: row)
        }
        .confirmationDialog(Text(verbatim: confirming?.text.title ?? ""), isPresented: Binding(get: { confirming != nil }, set: { if !$0 { confirming = nil } }),
                            titleVisibility: .visible, presenting: confirming) { c in
            Button(role: c.text.destructive ? .destructive : nil) {
                Task { await model.perform(c.action, section: path, row: c.row, parent: parent, values: c.values, confirmed: true) }
            } label: { Text(verbatim: c.text.button) }
        } message: { c in
            Text(verbatim: c.text.message)
        }
    }

    private func act(_ a: PanelAction, row: PanelRow?) {
        if !(a.fields ?? []).isEmpty {
            form = FormRequest(action: a, row: row)
        } else if let c = model.confirmation(a, row: row) {
            confirming = ConfirmRequest(action: a, row: row, text: c, values: [:])
        } else {
            Task { await model.perform(a, section: path, row: row, parent: parent) }
        }
    }
}

private struct PanelRowView: View {
    @ObservedObject var model: PanelModel
    let section: PanelSection
    let row: PanelRow
    let onAction: (PanelAction) -> Void
    let onOpen: (() -> Void)?

    var body: some View {
        let cols = section.columns ?? []
        let actions = model.rowActions(section, row)
        LabeledContent {
            HStack(spacing: 8) {
                if let onOpen { Button(action: onOpen) { Image(systemName: "chevron.right") }.buttonStyle(.borderless).help(String(localized: "Open")) }
                if !actions.isEmpty {
                    Menu {
                        ForEach(actions, id: \.id) { a in
                            Button(role: a.destructive == true || a.method == "DELETE" ? .destructive : nil) { onAction(a) } label: { Text(verbatim: model.text(a.label)) }
                        }
                    } label: { Image(systemName: "ellipsis.circle") }
                        .menuStyle(.borderlessButton).menuIndicator(.hidden).fixedSize().help(String(localized: "Actions"))
                }
            }
        } label: {
            ForEach(Array(cols.enumerated()), id: \.offset) { i, c in
                if !c.supported {
                    Text("This part of the panel cannot be shown.").font(.caption).foregroundStyle(.secondary)
                } else if i == 0 {
                    Text(verbatim: model.cell(c, row)).fixedSize(horizontal: false, vertical: true)
                } else {
                    Text(verbatim: model.text(c.label) + ": " + model.cell(c, row)).font(.caption).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }
}

private struct PanelFormSheet: View {
    @ObservedObject var model: PanelModel
    let action: PanelAction
    let row: PanelRow?
    let parent: PanelRow?
    let path: String
    @State private var values: [String: PanelJSON] = [:]
    @State private var confirming: PanelConfirmText?
    @State private var sending = false
    @Environment(\.dismiss) private var dismiss

    private var key: PanelListKey { PanelListKey(path: path, parent: parent?.id ?? "") }

    var body: some View {
        let problems = model.problems(action, values: values)
        VStack(spacing: 0) {
            Form {
                Section {
                    ForEach(action.fields ?? [], id: \.id) { f in
                        fieldView(f)
                        if let p = problems[f.id], values[f.id] != nil || p != .required {
                            switch p {
                            case .required: Text("Required").font(.caption).foregroundStyle(.red)
                            case .tooLong(let n): Text(String(localized: "At most \(n) characters")).font(.caption).foregroundStyle(.red)
                            }
                        }
                    }
                } header: { Text(verbatim: model.text(action.label)) }
                if let e = model.actionError[key] { Section { PanelErrorText(error: e) } }
            }
            .formStyle(.grouped)
            HStack {
                Spacer()
                EqualWidthHStack {
                    Button { dismiss() } label: { Text("Cancel").frame(maxWidth: .infinity) }.keyboardShortcut(.cancelAction)
                    Button { submit() } label: { Text("OK").frame(maxWidth: .infinity) }
                        .keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent).disabled(!problems.isEmpty || sending)
                }
            }
            .padding(16)
        }
        .frame(minWidth: 420, minHeight: 240)
        .onAppear { values = model.defaults(action, row: row) }
        .confirmationDialog(Text(verbatim: confirming?.title ?? ""), isPresented: Binding(get: { confirming != nil }, set: { if !$0 { confirming = nil } }),
                            titleVisibility: .visible, presenting: confirming) { c in
            Button(role: c.destructive ? .destructive : nil) { send(confirmed: true) } label: { Text(verbatim: c.button) }
        } message: { c in
            Text(verbatim: c.message)
        }
    }

    private func submit() {
        if let c = model.confirmation(action, row: row) { confirming = c } else { send(confirmed: false) }
    }

    private func send(confirmed: Bool) {
        sending = true
        Task {
            let ok = await model.perform(action, section: path, row: row, parent: parent, values: values, confirmed: confirmed)
            sending = false
            if ok { dismiss() }
        }
    }

    @ViewBuilder private func fieldView(_ f: PanelField) -> some View {
        let label = model.text(f.label)
        switch f.type {
        case "text":
            TextField(text: Binding(get: { if case .string(let s)? = values[f.id] { s } else { "" } }, set: { values[f.id] = .string($0) })) { Text(verbatim: label) }
        case "toggle":
            Toggle(isOn: Binding(get: { if case .bool(let b)? = values[f.id] { b } else { false } }, set: { values[f.id] = .bool($0) })) { Text(verbatim: label) }
        case "choice":
            Picker(selection: Binding(get: { if case .string(let s)? = values[f.id] { s } else { "" } }, set: { values[f.id] = $0.isEmpty ? nil : .string($0) })) {
                Text("None").tag("")
                ForEach(f.options ?? [], id: \.value) { o in Text(verbatim: model.text(o.label)).tag(o.value) }
            } label: { Text(verbatim: label) }
        default:  // ref
            let options = model.refs[f.ref ?? ""] ?? []
            Picker(selection: Binding(get: { values[f.id]?.scalarText ?? "" }, set: { id in values[f.id] = options.first { $0.id == id }?.key })) {
                Text("None").tag("")
                ForEach(options) { o in Text(verbatim: o.label).tag(o.id) }
            } label: { Text(verbatim: label) }
        }
    }
}

private struct PanelDetailSheet: View {
    @ObservedObject var model: PanelModel
    let section: PanelSection
    let row: PanelRow
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(spacing: 0) {
            Form {
                Section { Text(verbatim: row.values[section.detail?.title ?? ""]?.scalarText ?? row.id).font(.title2.bold()) }
                PanelSections(model: model, sections: section.detail?.sections ?? [], path: section.id + "/", parent: row)
            }
            .formStyle(.grouped)
            HStack { Spacer(); Button("Done") { dismiss() }.keyboardShortcut(.defaultAction) }.padding(16)
        }
        .frame(minWidth: 480, minHeight: 360)
        .task { await model.openDetail(section, row: row) }
        .onDisappear { model.closeDetail(section, row: row) }
    }
}
