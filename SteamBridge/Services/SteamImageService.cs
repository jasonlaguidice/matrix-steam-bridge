using Microsoft.Extensions.Logging;
using System.Text.Json;
using System.Security.Cryptography;
using System.Text;
using SteamKit2;
using SteamKit2.Internal;

namespace SteamBridge.Services;

public class SteamImageService
{
    private readonly ILogger<SteamImageService> _logger;
    private readonly SteamClientManager _steamClientManager;
    private readonly HttpClient _httpClient;
    
    public SteamImageService(
        ILogger<SteamImageService> logger,
        SteamClientManager steamClientManager,
        HttpClient httpClient)
    {
        _logger = logger;
        _steamClientManager = steamClientManager;
        _httpClient = httpClient;
    }

    /// <summary>
    /// Uploads an image to Steam UGC and returns a URL for sharing.
    /// Uses Steam's official UGC (User Generated Content) API to host images on steamusercontent.com.
    /// </summary>
    /// <param name="imageData">The image data bytes</param>
    /// <param name="mimeType">The MIME type (e.g., image/png, image/jpeg)</param>
    /// <param name="filename">The filename for the image</param>
    /// <returns>A URL for the uploaded image</returns>
    public async Task<string> UploadImageAsync(byte[] imageData, string mimeType, string filename)
    {
        // Enhanced connection state validation for SteamKit2 v3+ AsyncJob requirements
        if (!_steamClientManager.IsConnected)
        {
            throw new InvalidOperationException("Must be connected to Steam before uploading images. SteamKit2 v3+ AsyncJobs instantly fail if not connected.");
        }
        
        if (!_steamClientManager.IsLoggedOn)
        {
            throw new InvalidOperationException("Must be logged on to Steam before uploading images");
        }

        var unifiedMessages = _steamClientManager.SteamUnifiedMessages;
        if (unifiedMessages == null)
        {
            throw new InvalidOperationException("SteamUnifiedMessages is not available");
        }

        _logger.LogInformation("Starting Steam UGC upload: {Filename}, {MimeType}, {Size} bytes", 
            filename, mimeType, imageData.Length);

        try
        {
            // Create Cloud service instance using SteamKit2 v3+ API
            var cloudService = unifiedMessages.CreateService<SteamKit2.Internal.Cloud>();
            
            // Step 1: Begin UGC Upload
            var beginRequest = new CCloud_BeginUGCUpload_Request
            {
                appid = 0, // Steam Community uploads
                file_size = (uint)imageData.Length,
                filename = filename,
                file_sha = ComputeSHA1Hash(imageData),
                content_type = mimeType
            };

            _logger.LogDebug("Sending BeginUGCUpload request: {Filename}, SHA1: {SHA1}", 
                filename, beginRequest.file_sha);

            // Use the new SteamKit2 v3+ unified messaging API with timeout
            var beginJob = cloudService.BeginUGCUpload(beginRequest);
            
            // Add timeout to prevent indefinite hanging - SteamKit2 v3.2.0 AsyncJobs can fail instantly
            var beginTask = beginJob.ToTask();
            var timeoutTask = Task.Delay(TimeSpan.FromSeconds(30));
            var completedTask = await Task.WhenAny(beginTask, timeoutTask).ConfigureAwait(false);
            
            if (completedTask == timeoutTask)
            {
                throw new TimeoutException("Steam UGC BeginUpload request timed out after 30 seconds. This may indicate Steam server issues or client authentication problems.");
            }
            
            var beginResponse = await beginTask.ConfigureAwait(false);

            if (beginResponse?.Body == null)
            {
                throw new InvalidOperationException("BeginUGCUpload failed: No response received");
            }

            var beginResult = beginResponse.Body;
            if (beginResult == null)
            {
                throw new InvalidOperationException("BeginUGCUpload failed: Invalid response type");
            }

            _logger.LogDebug("BeginUGCUpload successful. UGC ID: {UGCID}, Host: {Host}, Path: {Path}", 
                beginResult.ugcid, beginResult.url_host, beginResult.url_path);

            // Step 2: HTTP Upload to Steam servers
            var uploadUrl = $"{(beginResult.use_https ? "https" : "http")}://{beginResult.url_host}{beginResult.url_path}";
            
            using var httpRequest = new HttpRequestMessage(HttpMethod.Put, uploadUrl);
            httpRequest.Content = new ByteArrayContent(imageData);
            httpRequest.Content.Headers.ContentType = new System.Net.Http.Headers.MediaTypeHeaderValue(mimeType);
            
            // Add required headers from Steam's response
            foreach (var header in beginResult.request_headers)
            {
                if (header.name.Equals("Content-Type", StringComparison.OrdinalIgnoreCase))
                    continue; // Already set above
                    
                httpRequest.Headers.TryAddWithoutValidation(header.name, header.value);
            }

            _logger.LogDebug("Uploading image data to Steam: {Url}", uploadUrl);
            
            using var httpResponse = await _httpClient.SendAsync(httpRequest).ConfigureAwait(false);
            var uploadSucceeded = httpResponse.IsSuccessStatusCode;
            
            if (!uploadSucceeded)
            {
                _logger.LogWarning("HTTP upload failed: {StatusCode} {ReasonPhrase}", 
                    httpResponse.StatusCode, httpResponse.ReasonPhrase);
            }
            else
            {
                _logger.LogDebug("HTTP upload successful: {StatusCode}", httpResponse.StatusCode);
            }

            // Step 3: Commit UGC Upload
            var commitRequest = new CCloud_CommitUGCUpload_Request
            {
                transfer_succeeded = uploadSucceeded,
                appid = 0,
                ugcid = beginResult.ugcid
            };

            _logger.LogDebug("Sending CommitUGCUpload request: UGC ID {UGCID}, Success: {Success}", 
                beginResult.ugcid, uploadSucceeded);

            // Use the new SteamKit2 v3+ unified messaging API with timeout
            var commitJob = cloudService.CommitUGCUpload(commitRequest);
            
            var commitTask = commitJob.ToTask();
            var commitTimeoutTask = Task.Delay(TimeSpan.FromSeconds(30));
            var completedCommitTask = await Task.WhenAny(commitTask, commitTimeoutTask).ConfigureAwait(false);
            
            if (completedCommitTask == commitTimeoutTask)
            {
                throw new TimeoutException("Steam UGC CommitUpload request timed out after 30 seconds.");
            }
            
            var commitResponse = await commitTask.ConfigureAwait(false);

            if (commitResponse?.Body == null)
            {
                throw new InvalidOperationException("CommitUGCUpload failed: No response received");
            }

            var commitResult = commitResponse.Body;
            if (commitResult == null)
            {
                throw new InvalidOperationException("CommitUGCUpload failed: Invalid response type");
            }

            if (!commitResult.file_committed)
            {
                throw new InvalidOperationException($"Steam UGC upload failed to commit: UGC ID {beginResult.ugcid}");
            }

            // Construct the final Steam UGC URL
            var ugcUrl = $"https://images.steamusercontent.com/ugc/{beginResult.ugcid}/{beginResult.ugcid}/";
            
            _logger.LogInformation("Steam UGC upload completed successfully: {Filename} -> {URL}", 
                filename, ugcUrl);

            return ugcUrl;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to upload image to Steam UGC: {Filename}", filename);
            throw;
        }
    }

    /// <summary>
    /// Downloads an image from a URL or processes a data URL.
    /// </summary>
    /// <param name="imageUrl">The image URL (HTTP/HTTPS or data URL)</param>
    /// <returns>A tuple containing the image data and MIME type</returns>
    public async Task<(byte[] data, string mimeType)> DownloadImageAsync(string imageUrl)
    {
        _logger.LogInformation("Processing image from URL: {Url}", 
            imageUrl.Length > 100 ? imageUrl.Substring(0, 100) + "..." : imageUrl);

        try
        {
            // Handle data URLs (e.g., data:image/png;base64,iVBORw0KGgoAAAA...)
            if (imageUrl.StartsWith("data:"))
            {
                return ProcessDataUrl(imageUrl);
            }

            // Handle regular HTTP/HTTPS URLs
            using var response = await _httpClient.GetAsync(imageUrl);
            response.EnsureSuccessStatusCode();

            var imageData = await response.Content.ReadAsByteArrayAsync();
            var mimeType = response.Content.Headers.ContentType?.MediaType ?? "image/jpeg";

            _logger.LogInformation("Image downloaded successfully: {Size} bytes, {MimeType}", 
                imageData.Length, mimeType);

            return (imageData, mimeType);
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to process image from URL: {Url}", 
                imageUrl.Length > 100 ? imageUrl.Substring(0, 100) + "..." : imageUrl);
            throw;
        }
    }

    /// <summary>
    /// Processes a data URL and extracts the image data and MIME type.
    /// </summary>
    /// <param name="dataUrl">The data URL (e.g., data:image/png;base64,iVBORw0KGgoAAAA...)</param>
    /// <returns>A tuple containing the image data and MIME type</returns>
    private (byte[] data, string mimeType) ProcessDataUrl(string dataUrl)
    {
        // Parse data URL format: data:image/png;base64,iVBORw0KGgoAAAA...
        const string dataPrefix = "data:";
        const string base64Marker = ";base64,";

        if (!dataUrl.StartsWith(dataPrefix))
        {
            throw new ArgumentException("Invalid data URL format", nameof(dataUrl));
        }

        var mimeTypeEnd = dataUrl.IndexOf(base64Marker);
        if (mimeTypeEnd == -1)
        {
            throw new ArgumentException("Data URL must contain base64 marker", nameof(dataUrl));
        }

        var mimeType = dataUrl.Substring(dataPrefix.Length, mimeTypeEnd - dataPrefix.Length);
        var base64Data = dataUrl.Substring(mimeTypeEnd + base64Marker.Length);

        try
        {
            var imageData = Convert.FromBase64String(base64Data);
            _logger.LogInformation("Data URL processed successfully: {Size} bytes, {MimeType}", 
                imageData.Length, mimeType);
            return (imageData, mimeType);
        }
        catch (FormatException ex)
        {
            throw new ArgumentException("Invalid base64 data in data URL", nameof(dataUrl), ex);
        }
    }

    /// <summary>
    /// Validates that the provided data is a supported image format.
    /// </summary>
    /// <param name="imageData">The image data to validate</param>
    /// <param name="mimeType">The claimed MIME type</param>
    /// <returns>True if the image is valid and supported</returns>
    public bool ValidateImage(byte[] imageData, string mimeType)
    {
        if (imageData == null || imageData.Length == 0)
        {
            _logger.LogWarning("Image validation failed: empty or null data");
            return false;
        }

        // Check size limits (Steam typically has a 10MB limit)
        const int maxSizeBytes = 10 * 1024 * 1024; // 10MB
        if (imageData.Length > maxSizeBytes)
        {
            _logger.LogWarning("Image validation failed: size {Size} exceeds limit {Limit}", 
                imageData.Length, maxSizeBytes);
            return false;
        }

        // Check supported MIME types
        var supportedTypes = new[] { "image/jpeg", "image/png", "image/gif", "image/webp" };
        if (!supportedTypes.Contains(mimeType?.ToLowerInvariant()))
        {
            _logger.LogWarning("Image validation failed: unsupported MIME type {MimeType}", mimeType);
            return false;
        }

        // Basic header validation
        if (!ValidateImageHeader(imageData, mimeType))
        {
            _logger.LogWarning("Image validation failed: invalid header for MIME type {MimeType}", mimeType);
            return false;
        }

        return true;
    }

    private bool ValidateImageHeader(byte[] imageData, string mimeType)
    {
        if (imageData.Length < 4) return false;

        return mimeType?.ToLowerInvariant() switch
        {
            "image/jpeg" => imageData[0] == 0xFF && imageData[1] == 0xD8,
            "image/png" => imageData[0] == 0x89 && imageData[1] == 0x50 && 
                          imageData[2] == 0x4E && imageData[3] == 0x47,
            "image/gif" => (imageData[0] == 0x47 && imageData[1] == 0x49 && 
                           imageData[2] == 0x46 && imageData[3] == 0x38),
            "image/webp" => imageData.Length >= 12 && 
                           imageData[0] == 0x52 && imageData[1] == 0x49 && 
                           imageData[2] == 0x46 && imageData[3] == 0x46 &&
                           imageData[8] == 0x57 && imageData[9] == 0x45 && 
                           imageData[10] == 0x42 && imageData[11] == 0x50,
            _ => false
        };
    }

    /// <summary>
    /// Computes SHA1 hash of image data for Steam UGC upload.
    /// Steam requires a SHA1 hash of the file content for verification.
    /// </summary>
    /// <param name="data">The image data bytes</param>
    /// <returns>SHA1 hash as lowercase hex string</returns>
    private static string ComputeSHA1Hash(byte[] data)
    {
        using var sha1 = SHA1.Create();
        var hash = sha1.ComputeHash(data);
        return Convert.ToHexString(hash).ToLowerInvariant();
    }

    private string GetFileExtension(string mimeType)
    {
        return mimeType?.ToLowerInvariant() switch
        {
            "image/jpeg" => "jpg",
            "image/png" => "png", 
            "image/gif" => "gif",
            "image/webp" => "webp",
            _ => "jpg"
        };
    }
}