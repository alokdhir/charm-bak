import Foundation

// The socket wire format. docs/protocol.md is the contract; the Go side is
// internal/protocol; testdata/protocol has one example per method, error
// and event, and both sides' tests decode them. Field names are camelCase
// here and snake_case on the wire (see Wire.encoder/decoder).

public enum ProtocolInfo {
    public static let version = 1

    /// ~/Library/Application Support/charm-bak/app.sock
    public static var socketPath: String {
        supportDir.appending(path: "app.sock").path
    }

    /// ~/Library/Application Support/charm-bak, created owner-only.
    public static var supportDir: URL {
        let dir = FileManager.default.homeDirectoryForCurrentUser
            .appending(path: "Library/Application Support/charm-bak")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        // Owner-only even if an older build created it wider.
        try? FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: dir.path)
        return dir
    }
}

public enum ErrorCode: String, Codable, Sendable {
    case badRequest = "bad_request"
    case unknownMethod = "unknown_method"
    case unsupportedProtocol = "unsupported_protocol"
    case notFound = "not_found"
    case busy
    case resticFailed = "restic_failed"
    case notConfigured = "not_configured"
    case `internal`
    case hostUnreachable = "host_unreachable"
}

/// A failed request. `message` is a sentence the TUI may show as is.
public struct ProtocolError: Error, Sendable {
    public var code: ErrorCode
    public var message: String
    public var data: (any Encodable & Sendable)?

    public init(_ code: ErrorCode, _ message: String, data: (any Encodable & Sendable)? = nil) {
        self.code = code
        self.message = message
        self.data = data
    }
}

public struct BusyData: Codable, Sendable { public var job: Job }
public struct UnsupportedProtocolData: Codable, Sendable { public var `protocol`: Int }
public struct ResticFailedData: Codable, Sendable { public var exit: Int32; public var stderr: String }

public struct Retention: Codable, Sendable, Equatable {
    public var last, hourly, daily, weekly, monthly, yearly: Int
}

public struct Schedule: Codable, Sendable, Equatable {
    public var every: Int64
}

public struct Source: Codable, Sendable {
    public var id: String
    public var name: String
    public var host: String
    public var paths: [String]
    public var excludes: [String]
    public var schedule: Schedule?
    public var retention: Retention
    public var inherited: [String]
    public var paused: Bool
    public var health: String
    public var files: Int64
    public var bytes: Int64
    public var lastRun: Run?
    public var nextRun: Date?
}

public struct Snapshot: Codable, Sendable {
    public var id: String
    public var shortId: String
    public var time: Date
    public var end: Date?
    public var paths: [String]
    public var hostname: String
    public var tags: [String]
    public var files: Int64
    public var bytes: Int64
    public var bytesAdded: Int64
    public var keptBy: [String]
    public var pinned: Bool
    public var note: String
}

public struct Entry: Codable, Sendable {
    public var name: String
    public var type: String
    public var size: Int64
    public var mode: String
    public var mtime: Date
}

public struct Job: Codable, Sendable {
    public var id: String
    public var kind: String
    public var source: String?
    public var started: Date
}

public struct Run: Codable, Sendable {
    public var job: String
    public var kind: String
    public var source: String?
    public var started: Date
    public var ended: Date
    public var status: String
    public var filesNew: Int64
    public var filesChanged: Int64
    public var bytesAdded: Int64
    public var files: Int64
    public var bytes: Int64
    public var snapshot: String
    public var error: String
}

public enum AppState: String, Codable, Sendable {
    case idle, running, pruning, paused
}

public struct Repository: Codable, Sendable {
    public var location: String
    public var storedBytes: Int64
    public var logicalBytes: Int64
}

// MARK: methods

public struct HelloParams: Codable, Sendable { public var `protocol`: Int; public var client: String }
public struct HelloResult: Codable, Sendable {
    public var `protocol`: Int
    public var app: String
    public var build: String
    public var os: String
    public var arch: String
    public var bundle: Bool?
}

public struct StatusResult: Codable, Sendable {
    public var repository: Repository?
    public var state: AppState
    public var job: Job?
    public var paused: Bool
    public var fullDiskAccess: Bool?
}

public struct SourcesResult: Codable, Sendable { public var sources: [Source] }
public struct SnapshotsParams: Codable, Sendable { public var source: String }
public struct SnapshotsResult: Codable, Sendable { public var snapshots: [Snapshot] }

public struct LsParams: Codable, Sendable {
    public var snapshot: String
    public var path: String
    public var offset: Int?
    public var limit: Int?
}

public struct LsResult: Codable, Sendable {
    public var entries: [Entry]
    public var total: Int
    public var nextOffset: Int?
}

public struct DumpParams: Codable, Sendable { public var snapshot: String; public var path: String; public var max: Int }

/// `data` is base64 on the wire, which Data's Codable does.
public struct DumpResult: Codable, Sendable {
    public var data: Data
    public var size: Int64
    public var truncated: Bool
    public var binary: Bool
}

public struct DiffParams: Codable, Sendable { public var snapshot: String; public var path: String }
public struct DiffEntry: Codable, Sendable { public var name: String; public var type: String; public var change: String }
public struct DiffResult: Codable, Sendable { public var entries: [DiffEntry] }
public struct ChangesParams: Codable, Sendable { public var snapshot: String; public var base: String }
public struct PathChange: Codable, Sendable { public var path: String; public var change: String }
public struct ChangesResult: Codable, Sendable {
    public var changes: [PathChange]
    public var added: Int
    public var removed: Int
    public var changed: Int
    public var touched: Int
    public var bytesAdded: Int64
}

public struct RestoreParams: Codable, Sendable {
    public var snapshot: String
    public var paths: [String]
    public var target: String
    public var overwrite: String
    /// A paired remote whose repository to restore from, onto the machine
    /// handling the request.
    public var from: String?
}

public struct JobResult: Codable, Sendable { public var job: Job }
public struct BackupNowParams: Codable, Sendable { public var source: String }
public struct CancelParams: Codable, Sendable { public var job: String }

public struct SourceAddParams: Codable, Sendable {
    public var name: String
    public var paths: [String]
    public var excludes: [String]
    public var schedule: Schedule?
    public var retention: Retention?
    public var backUpNow: Bool
}

/// Takes paths out of a source's existing snapshots (a rewrite job, then a prune).
public struct SourceRewriteParams: Codable, Sendable {
    public var id: String
    public var excludes: [String]
    public var dryRun: Bool?
}
public struct SourceRewriteResult: Codable, Sendable { public var snapshots: Int; public var job: Job? }
public struct SourceSetParams: Codable, Sendable {
    public var id: String
    public var name: String?
    public var paths: [String]?
    public var excludes: [String]?
    public var schedule: Schedule?
    public var retention: Retention?
    public var paused: Bool?
    /// Fields to reset to the defaults: "schedule", "retention".
    public var inherit: [String]?
}

public struct SourceResult: Codable, Sendable { public var source: Source }
public struct SourceRemoveParams: Codable, Sendable { public var id: String; public var keepSnapshots: Bool }
public struct SnapshotSetParams: Codable, Sendable { public var id: String; public var note: String?; public var pinned: Bool? }
public struct SnapshotMetaParams: Codable, Sendable { public var source: String }
public struct SnapshotMetaResult: Codable, Sendable {
    public var pinned: [String]
    public var notes: [String: String]
    public var added: [String: Int64]
}
public struct SnapshotResult: Codable, Sendable { public var snapshot: Snapshot }
public struct HistoryParams: Codable, Sendable { public var since: Date }
public struct HistoryResult: Codable, Sendable { public var runs: [Run] }

public struct RepositoryInfo: Codable, Sendable {
    public var location: String
    public var passwordFrom: String
    public var awsFrom: String
    public var storedBytes: Int64
    public var logicalBytes: Int64
    public var lastPrune: Run?
    public var lastCheck: Run?
    public var nextMaintenance: Date?
}
public struct Defaults: Codable, Sendable {
    public var schedule: Schedule?
    public var retention: Retention
    public var excludes: [String]
}
public struct RepositoryResult: Codable, Sendable { public var repository: RepositoryInfo?; public var defaults: Defaults }
public struct DefaultsSetParams: Codable, Sendable { public var schedule: Schedule?; public var retention: Retention?; public var excludes: [String]? }
public struct DefaultsResult: Codable, Sendable { public var defaults: Defaults }
public struct Host: Codable, Sendable {
    public var name: String
    public var local: Bool
    public var address: String
    public var state: String
    public var error: String
    public var lastSeen: Date?
}
public struct HostsResult: Codable, Sendable { public var hosts: [Host] }
public struct PairStartParams: Codable, Sendable {
    public var name: String
    public init(name: String) { self.name = name }
}
public struct PairStartResult: Codable, Sendable { public var command: String; public var expires: Date }
/// The command that reinstalls a paired remote's daemon there, keeping its state.
public struct HostRepairParams: Codable, Sendable { public var name: String }
/// Sent by a controller's daemon to a remote; clients never hold these.
public struct RepositorySetParams: Codable, Sendable {
    public var location: String
    public var password: String
    public var awsKey: String
    public var `init`: Bool
    public var env: [String: String]?
}
public struct RepositoryProbeParams: Codable, Sendable {
    public var location: String
    public var env: [String: String]?
    public var password: String?
}
public struct RepositoryProbeResult: Codable, Sendable { public var state: String; public var message: String? }
public struct RepositorySetupParams: Codable, Sendable {
    public var host: String
    public var location: String
    public var env: [String: String]?
    public var password: String?
    public var `init`: Bool
}
public struct RepositorySetupResult: Codable, Sendable { public var repository: RepositoryInfo?; public var password: String? }
/// Daemon to daemon only: a new executable for a remote, in pieces.
public struct SelfUpdateParams: Codable, Sendable {
    public var sha256: String
    public var size: Int64
    public var offset: Int64
    public var data: Data
    public var bundle: Bool?
}
public struct SelfUpdateResult: Codable, Sendable { public var received: Int64; public var restarting: Bool }
/// A paired remote's new address; the pairing stays.
public struct HostSetParams: Codable, Sendable {
    public var name: String
    public var address: String
    public init(name: String, address: String) { self.name = name; self.address = address }
}
public struct HostResult: Codable, Sendable { public var host: Host }
/// Removes a paired remote: it uninstalls itself, then it's unpaired.
public struct HostRemoveParams: Codable, Sendable {
    public var name: String
    public var deleteBackups: Bool?
}
public struct HostRemoveResult: Codable, Sendable {
    public var uninstalled: Bool
    public var message: String?
}
public struct UninstallParams: Codable, Sendable {
    public var deleteBackups: Bool?
}
/// What the Mac's daemon can do in AWS by itself (aws_status).
public struct AWSStatusResult: Codable, Sendable {
    public var cli: Bool
    public var manager: AWSManager?
}
public struct AWSManager: Codable, Sendable {
    public var account: String
    public var bucket: String
    public var region: String
}
/// A key that only reaches the host's folder in the bucket (aws_keys).
public struct AWSKeysParams: Codable, Sendable {
    public var host: String
    public var bucket: String?
    public var region: String?
    public var signIn: Bool
}
/// How far aws_keys has got making a machine's key.
public struct AWSStep: Codable, Sendable {
    public var `for`: String
    public var step: String
    public var account: String?
    public var bucket: String?
}
public struct AWSKeysResult: Codable, Sendable {
    public var bucket: String
    public var region: String
    public var env: [String: String]
    public var bucketCreated: Bool?
}
public struct Empty: Codable, Sendable { public init() {} }

public enum Method: String, CaseIterable, Sendable {
    case hello, status, sources, snapshots, ls, dump, diff, changes, restore
    case backupNow = "backup_now"
    case cancel
    case sourceAdd = "source_add"
    case sourceSet = "source_set"
    case sourceRewrite = "source_rewrite"
    case sourceRemove = "source_remove"
    case snapshotSet = "snapshot_set"
    case snapshotMeta = "snapshot_meta"
    case pause, resume, history, subscribe, repository
    case defaultsSet = "defaults_set"
    case hosts
    case pairStart = "pair_start"
    case hostRepair = "host_repair"
    case repositorySet = "repository_set"
    case selfUpdate = "self_update"
    case hostSet = "host_set"
    case repositoryProbe = "repository_probe"
    case repositorySetup = "repository_setup"
    case hostRemove = "host_remove"
    case uninstall
    case accessSettings = "access_settings"
    case awsStatus = "aws_status"
    case awsKeys = "aws_keys"

    /// Each method's params and result types; nil params = takes none.
    public var types: (params: (any Decodable.Type)?, result: any Decodable.Type) {
        switch self {
        case .hello: (HelloParams.self, HelloResult.self)
        case .status: (nil, StatusResult.self)
        case .sources: (nil, SourcesResult.self)
        case .snapshots: (SnapshotsParams.self, SnapshotsResult.self)
        case .ls: (LsParams.self, LsResult.self)
        case .dump: (DumpParams.self, DumpResult.self)
        case .diff: (DiffParams.self, DiffResult.self)
        case .changes: (ChangesParams.self, ChangesResult.self)
        case .restore: (RestoreParams.self, JobResult.self)
        case .backupNow: (BackupNowParams.self, JobResult.self)
        case .cancel: (CancelParams.self, Empty.self)
        case .sourceAdd: (SourceAddParams.self, SourceResult.self)
        case .sourceSet: (SourceSetParams.self, SourceResult.self)
        case .sourceRewrite: (SourceRewriteParams.self, SourceRewriteResult.self)
        case .sourceRemove: (SourceRemoveParams.self, Empty.self)
        case .snapshotSet: (SnapshotSetParams.self, SnapshotResult.self)
        case .snapshotMeta: (SnapshotMetaParams.self, SnapshotMetaResult.self)
        case .pause, .resume, .subscribe: (nil, Empty.self)
        case .history: (HistoryParams.self, HistoryResult.self)
        case .repository: (nil, RepositoryResult.self)
        case .defaultsSet: (DefaultsSetParams.self, DefaultsResult.self)
        case .hosts: (nil, HostsResult.self)
        case .pairStart: (PairStartParams.self, PairStartResult.self)
        case .hostRepair: (HostRepairParams.self, PairStartResult.self)
        case .repositorySet: (RepositorySetParams.self, RepositoryResult.self)
        case .selfUpdate: (SelfUpdateParams.self, SelfUpdateResult.self)
        case .hostSet: (HostSetParams.self, HostResult.self)
        case .repositoryProbe: (RepositoryProbeParams.self, RepositoryProbeResult.self)
        case .repositorySetup: (RepositorySetupParams.self, RepositorySetupResult.self)
        case .hostRemove: (HostRemoveParams.self, HostRemoveResult.self)
        case .uninstall: (UninstallParams.self, Empty.self)
        case .accessSettings: (nil, Empty.self)
        case .awsStatus: (nil, AWSStatusResult.self)
        case .awsKeys: (AWSKeysParams.self, AWSKeysResult.self)
        }
    }
}

// MARK: events

public struct Progress: Codable, Sendable {
    public var jobId: String
    public var percent: Double
    public var filesDone: Int64
    public var totalFiles: Int64
    public var bytesDone: Int64
    public var totalBytes: Int64
    public var current: String
}

public enum Event: Sendable {
    case jobStarted(Job)
    case progress(Progress)
    case jobFinished(Run)
    case sourceChanged(Source)
    case stateChanged(AppState, Job?)
    case hostChanged(Host)
    case awsStep(AWSStep)

    public var name: String {
        switch self {
        case .jobStarted: "job_started"
        case .progress: "progress"
        case .jobFinished: "job_finished"
        case .sourceChanged: "source_changed"
        case .stateChanged: "state_changed"
        case .hostChanged: "host_changed"
        case .awsStep: "aws_step"
        }
    }

    private struct JobPayload: Codable { var job: Job }
    private struct RunPayload: Codable { var run: Run }
    private struct SourcePayload: Codable { var source: Source }
    private struct StatePayload: Codable { var state: AppState; var job: Job? }
    private struct HostPayload: Codable { var hostInfo: Host }
    private struct Head: Codable { var event: String }

    /// One wire line's object: the payload's fields with "event" beside them.
    public func encoded() throws -> Data {
        let payload: Data = switch self {
        case .jobStarted(let j): try Wire.encoder.encode(JobPayload(job: j))
        case .progress(let p): try Wire.encoder.encode(p)
        case .jobFinished(let r): try Wire.encoder.encode(RunPayload(run: r))
        case .sourceChanged(let s): try Wire.encoder.encode(SourcePayload(source: s))
        case .stateChanged(let s, let j): try Wire.encoder.encode(StatePayload(state: s, job: j))
        case .hostChanged(let h): try Wire.encoder.encode(HostPayload(hostInfo: h))
        case .awsStep(let s): try Wire.encoder.encode(s)
        }
        var object = try JSONSerialization.jsonObject(with: payload) as? [String: Any] ?? [:]
        object["event"] = name
        return try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
    }

    private struct HostHead: Codable { var host: String? }

    /// The remote an event came from; nil for the local daemon's.
    public static func host(_ data: Data) -> String? {
        (try? Wire.decoder.decode(HostHead.self, from: data))?.host
    }

    /// Decodes an event object; nil for an event this app doesn't know.
    public static func decode(_ data: Data) throws -> Event? {
        let head = try Wire.decoder.decode(Head.self, from: data)
        switch head.event {
        case "job_started": return .jobStarted(try Wire.decoder.decode(JobPayload.self, from: data).job)
        case "progress": return .progress(try Wire.decoder.decode(Progress.self, from: data))
        case "job_finished": return .jobFinished(try Wire.decoder.decode(RunPayload.self, from: data).run)
        case "source_changed": return .sourceChanged(try Wire.decoder.decode(SourcePayload.self, from: data).source)
        case "state_changed":
            let p = try Wire.decoder.decode(StatePayload.self, from: data)
            return .stateChanged(p.state, p.job)
        case "host_changed": return .hostChanged(try Wire.decoder.decode(HostPayload.self, from: data).hostInfo)
        case "aws_step": return .awsStep(try Wire.decoder.decode(AWSStep.self, from: data))
        default: return nil
        }
    }
}

// MARK: JSON

public enum Wire {
    /// RFC 3339 with the local offset, e.g. 2026-11-19T12:57:13-05:00.
    private static func formatter(fractional: Bool) -> ISO8601DateFormatter {
        let f = ISO8601DateFormatter()
        f.timeZone = .current
        f.formatOptions = fractional ? [.withInternetDateTime, .withFractionalSeconds] : [.withInternetDateTime]
        return f
    }

    /// Parses RFC 3339 with any number of fractional digits (restic writes
    /// nanoseconds; ISO8601DateFormatter only copes with milliseconds).
    public static func parseTime(_ s: String) -> Date? {
        guard let dot = s.firstIndex(of: "."), let t = s.firstIndex(of: "T"), dot > t else {
            return formatter(fractional: false).date(from: s)
        }
        let digitsEnd = s[s.index(after: dot)...].firstIndex { !$0.isNumber } ?? s.endIndex
        let digits = s[s.index(after: dot)..<digitsEnd]
        guard let base = formatter(fractional: false).date(from: String(s[..<dot]) + String(s[digitsEnd...])) else {
            return nil
        }
        return base.addingTimeInterval(Double("0." + digits) ?? 0)
    }

    public static var encoder: JSONEncoder {
        let e = JSONEncoder()
        e.keyEncodingStrategy = .convertToSnakeCase
        e.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        e.dateEncodingStrategy = .custom { date, enc in
            var c = enc.singleValueContainer()
            try c.encode(formatter(fractional: false).string(from: date))
        }
        return e
    }

    public static var decoder: JSONDecoder {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            if let date = parseTime(s) {
                return date
            }
            throw DecodingError.dataCorrupted(.init(codingPath: dec.codingPath, debugDescription: "not RFC 3339: \(s)"))
        }
        return d
    }
}
