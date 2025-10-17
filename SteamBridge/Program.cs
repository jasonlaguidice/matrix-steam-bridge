using SteamBridge.Services;
using Microsoft.AspNetCore.Builder;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
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
builder.Services.AddLogging();

// Add HttpClient for Steam Web API calls
builder.Services.AddHttpClient();

// Configure Steam services
builder.Services.AddSingleton<SteamClientManager>();
builder.Services.AddSingleton<SteamAuthenticationService>();
builder.Services.AddSingleton<SteamUserInformationService>();
builder.Services.AddSingleton<SteamMessagingManager>();
builder.Services.AddSingleton<SteamSessionManager>();
builder.Services.AddSingleton<SteamImageService>();

// Add parent process monitoring service
builder.Services.AddHostedService<ParentProcessMonitorService>();

var app = builder.Build();

// Configure the HTTP request pipeline
app.MapGrpcService<SteamAuthService>();
app.MapGrpcService<SteamUserService>();
app.MapGrpcService<SteamMessagingService>();
app.MapGrpcService<SteamSessionService>();
app.MapGrpcService<SteamPresenceService>();

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
