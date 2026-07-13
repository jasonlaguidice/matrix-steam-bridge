using SteamBridge.Services;
using Microsoft.AspNetCore.Builder;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.AspNetCore.Server.Kestrel.Core;

var builder = WebApplication.CreateBuilder(args);

// Configure Kestrel to use HTTP/2 for gRPC
builder.Services.Configure<KestrelServerOptions>(options =>
{
    options.ListenLocalhost(50051, listenOptions =>
    {
        listenOptions.Protocols = HttpProtocols.Http2;
    });
});

// Add services to the container
builder.Services.AddGrpc();
builder.Services.AddGrpcHealthChecks()
    .AddCheck("self", () => Microsoft.Extensions.Diagnostics.HealthChecks.HealthCheckResult.Healthy("SteamBridge is running"));

// Without an explicit minimum level, ASP.NET Core's Production-hosting default is
// Information - LogDebug calls (e.g. persona state changes, rich presence request
// triggers) would be silently dropped before ever reaching stdout, regardless of the Go
// bridge's own log level. Track the Go bridge's own effective level automatically (it
// sets STEAM_BRIDGE_LOG_LEVEL from its own config.yaml logging.min_level when spawning
// this process - see connector.go's startSteamBridgeService) instead of hardcoding one
// here, so the two never drift out of sync. Keep the ASP.NET Core framework's own
// internals (routing, hosting diagnostics) quieter regardless, so enabling Debug/Trace
// doesn't flood the log with unrelated noise.
var minLogLevel = MapZerologLevel(Environment.GetEnvironmentVariable("STEAM_BRIDGE_LOG_LEVEL"));
builder.Services.AddLogging(logging =>
{
    logging.SetMinimumLevel(minLogLevel);
    logging.AddFilter("Microsoft", LogLevel.Warning);
    logging.AddFilter("System", LogLevel.Warning);
});

// Maps a zerolog level string (as produced by Go's zerolog.Level.String() - "trace",
// "debug", "info", "warn", "error", "fatal", "panic") to the closest .NET LogLevel.
// Falls back to Information (ASP.NET Core's own Production default) for null/unset/
// unrecognized values, so a missing env var behaves the same as before this change.
static LogLevel MapZerologLevel(string? zerologLevel) => zerologLevel switch
{
    "trace" => LogLevel.Trace,
    "debug" => LogLevel.Debug,
    "info" => LogLevel.Information,
    "warn" => LogLevel.Warning,
    "error" => LogLevel.Error,
    "fatal" => LogLevel.Critical,
    "panic" => LogLevel.Critical,
    _ => LogLevel.Information,
};

// Add HttpClient for Steam Web API calls
builder.Services.AddHttpClient();

// Configure Steam services
builder.Services.AddSingleton<SteamClientRegistry>();
builder.Services.AddSingleton<SteamAuthenticationService>();
builder.Services.AddSingleton<SteamUserInformationService>();
builder.Services.AddSingleton<SteamImageService>();
builder.Services.AddSingleton<RichPresenceLocalizationService>();
builder.Services.AddSingleton<SteamAppInfoService>();

// Add parent process monitoring service
builder.Services.AddHostedService<ParentProcessMonitorService>();

var app = builder.Build();

// Configure the HTTP request pipeline
app.MapGrpcService<SteamAuthService>();
app.MapGrpcService<SteamUserService>();
app.MapGrpcService<SteamMessagingService>();
app.MapGrpcService<SteamSessionService>();
app.MapGrpcService<SteamPresenceService>();
app.MapGrpcService<SteamGroupService>();

// Map standard gRPC health service
app.MapGrpcHealthChecksService();

app.MapGet("/", () => "Steam Bridge gRPC Service is running. Use a gRPC client to connect.");

var parentPid = Environment.GetEnvironmentVariable("PARENT_PID");
if (!string.IsNullOrEmpty(parentPid))
{
    Console.WriteLine($"Steam Bridge starting on localhost:50051 (HTTP/2) with parent process monitoring (PID: {parentPid})");
}
else
{
    Console.WriteLine("Steam Bridge starting on localhost:50051 (HTTP/2)");
}

app.Run();
