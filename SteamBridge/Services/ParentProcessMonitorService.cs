using System.Diagnostics;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace SteamBridge.Services;

/// <summary>
/// Background service that monitors the parent process and gracefully shuts down the service
/// if the parent process terminates. This prevents orphaned SteamBridge processes.
/// </summary>
public class ParentProcessMonitorService : BackgroundService
{
    private readonly ILogger<ParentProcessMonitorService> _logger;
    private readonly IHostApplicationLifetime _applicationLifetime;
    private readonly int? _parentProcessId;
    private readonly TimeSpan _checkInterval = TimeSpan.FromSeconds(5);

    public ParentProcessMonitorService(
        ILogger<ParentProcessMonitorService> logger,
        IHostApplicationLifetime applicationLifetime)
    {
        _logger = logger;
        _applicationLifetime = applicationLifetime;

        // Check for PARENT_PID environment variable
        var parentPidString = Environment.GetEnvironmentVariable("PARENT_PID");
        if (!string.IsNullOrEmpty(parentPidString) && int.TryParse(parentPidString, out var parentPid))
        {
            _parentProcessId = parentPid;
            _logger.LogInformation("Parent process monitoring enabled for PID: {ParentPid}", _parentProcessId);
        }
        else
        {
            _parentProcessId = null;
            _logger.LogInformation("No PARENT_PID environment variable found. Parent process monitoring disabled.");
        }
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        // If no parent process ID is configured, exit early
        if (!_parentProcessId.HasValue)
        {
            _logger.LogInformation("Parent process monitoring is disabled.");
            return;
        }

        _logger.LogInformation("Starting parent process monitoring for PID: {ParentPid}", _parentProcessId.Value);

        try
        {
            // Initial check to ensure parent process exists
            if (!IsProcessRunning(_parentProcessId.Value))
            {
                _logger.LogWarning("Parent process {ParentPid} is not running at startup. Shutting down immediately.", _parentProcessId.Value);
                _applicationLifetime.StopApplication();
                return;
            }

            // Monitor parent process every 5 seconds
            while (!stoppingToken.IsCancellationRequested)
            {
                try
                {
                    await Task.Delay(_checkInterval, stoppingToken);

                    if (!IsProcessRunning(_parentProcessId.Value))
                    {
                        _logger.LogWarning("Parent process {ParentPid} has terminated. Initiating graceful shutdown.", _parentProcessId.Value);
                        _applicationLifetime.StopApplication();
                        break;
                    }

                    _logger.LogDebug("Parent process {ParentPid} is still running.", _parentProcessId.Value);
                }
                catch (OperationCanceledException)
                {
                    // Service is being stopped normally
                    _logger.LogInformation("Parent process monitoring is stopping due to service shutdown.");
                    break;
                }
                catch (Exception ex)
                {
                    _logger.LogError(ex, "Error occurred while monitoring parent process {ParentPid}", _parentProcessId.Value);
                    // Continue monitoring despite errors
                }
            }
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Fatal error in parent process monitoring service");
            // If monitoring fails critically, shut down the service
            _applicationLifetime.StopApplication();
        }

        _logger.LogInformation("Parent process monitoring has stopped.");
    }

    /// <summary>
    /// Checks if a process with the specified ID is currently running.
    /// </summary>
    /// <param name="processId">The process ID to check</param>
    /// <returns>True if the process is running, false otherwise</returns>
    private bool IsProcessRunning(int processId)
    {
        try
        {
            using var process = Process.GetProcessById(processId);
            // If we can get the process and it hasn't exited, it's running
            return !process.HasExited;
        }
        catch (ArgumentException)
        {
            // Process with the specified ID does not exist
            return false;
        }
        catch (InvalidOperationException)
        {
            // Process has already exited
            return false;
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Unable to check if process {ProcessId} is running. Assuming it's not running.", processId);
            return false;
        }
    }
}