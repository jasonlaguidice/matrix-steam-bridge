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
    /// Uploads an image and returns a URL for sharing.
    /// Note: This is a placeholder implementation. Steam's UGC upload API requires
    /// more complex SteamKit2 integration with proper callback handling.
    /// For production use, consider integrating with a reliable image hosting service.
    /// </summary>
    /// <param name="imageData">The image data bytes</param>
    /// <param name="mimeType">The MIME type (e.g., image/png, image/jpeg)</param>
    /// <param name="filename">The filename for the image</param>
    /// <returns>A URL for the uploaded image</returns>
    public async Task<string> UploadImageAsync(byte[] imageData, string mimeType, string filename)
    {
        if (!_steamClientManager.IsLoggedOn)
        {
            throw new InvalidOperationException("Must be logged on to Steam before uploading images");
        }

        _logger.LogInformation("Processing image upload: {Filename}, {MimeType}, {Size} bytes", 
            filename, mimeType, imageData.Length);

        try
        {
            // TODO: Implement Steam UGC upload when SteamKit2 callback system is integrated
            // For now, we'll create a data URL that includes the image data
            // This allows the system to work end-to-end while we work on the UGC integration
            
            var base64Data = Convert.ToBase64String(imageData);
            var dataUrl = $"data:{mimeType};base64,{base64Data}";
            
            _logger.LogInformation("Image processed as data URL: {Filename}, {Size} bytes", filename, imageData.Length);
            
            /* STEAM UGC UPLOAD IMPLEMENTATION NOTES FOR FUTURE DEVELOPMENT:
             * 
             * Steam provides a UGC (User Generated Content) upload API that can host images
             * on steamusercontent.com. The process involves 3 steps:
             * 
             * STEP 1: BeginUGCUpload
             * - Send CCloud_BeginUGCUpload_Request via ServiceMethodCallFromClient
             * - Required fields:
             *   - appid: 0 (for general Steam Community uploads)
             *   - file_size: size of image in bytes
             *   - filename: original filename
             *   - file_sha: SHA1 hash of file content (lowercase hex)
             *   - content_type: MIME type (e.g., "image/png")
             * - Response: CCloud_BeginUGCUpload_Response contains:
             *   - ugcid: unique identifier for the upload
             *   - url_host: hostname for upload (e.g., "steamcloud-ugc.akamaized.net")
             *   - url_path: path for upload (e.g., "/ugc-upload/...")
             *   - use_https: whether to use HTTPS
             *   - request_headers: additional headers required for upload
             * 
             * STEP 2: HTTP Upload
             * - Upload file data via HTTP PUT to: {protocol}://{url_host}{url_path}
             * - Content-Type: set to the image MIME type
             * - Include all headers from request_headers in the HTTP request
             * - Body: raw image data bytes
             * 
             * STEP 3: CommitUGCUpload
             * - Send CCloud_CommitUGCUpload_Request via ServiceMethodCallFromClient
             * - Required fields:
             *   - transfer_succeeded: true if HTTP upload succeeded
             *   - appid: 0 (same as BeginUGCUpload)
             *   - ugcid: the ugcid from BeginUGCUpload response
             * - Response: CCloud_CommitUGCUpload_Response confirms completion
             * 
             * FINAL URL CONSTRUCTION:
             * The resulting image URL follows the pattern:
             * https://images.steamusercontent.com/ugc/{ugcid}/{hash}/
             * 
             * Where:
             * - ugcid: the unique ID from Steam's response
             * - hash: appears to be related to file content or timestamp
             * 
             * STEAMKIT2 INTEGRATION REQUIREMENTS:
             * 1. Implement proper callback handling for ServiceMethodCallFromClient
             * 2. Add message handlers for CCloud_BeginUGCUpload_Response
             * 3. Add message handlers for CCloud_CommitUGCUpload_Response
             * 4. Handle async response correlation using JobIDs
             * 5. Add proper error handling for upload failures
             * 
             * EXAMPLE USAGE IN STEAMKIT2:
             * var request = new ClientMsgProtobuf<CCloud_BeginUGCUpload_Request>(EMsg.ServiceMethodCallFromClient);
             * request.Header.realm = 1;  // Steam realm
             * request.Header.target_job_name = "Cloud.BeginUGCUpload#1";
             * request.SourceJobID = client.GetNextJobID();
             * request.Body.appid = 0;
             * request.Body.file_size = (uint)imageData.Length;
             * request.Body.filename = filename;
             * request.Body.file_sha = ComputeSHA1Hash(imageData);
             * request.Body.content_type = mimeType;
             * client.Send(request);
             * 
             * REFERENCES:
             * - Steam Web Client uploads to steamusercontent.com via this method
             * - UGC messages defined in SteamKit2/Base/Generated/SteamMsgCloud.cs
             * - mx-puppet-steam project showed evidence of similar upload capability
             * 
             * BENEFITS OF STEAM UGC APPROACH:
             * - Images hosted on Steam's official CDN
             * - Permanent URLs that don't expire
             * - High availability and global distribution
             * - Native integration with Steam ecosystem
             * - No third-party dependencies
             */
            
            return dataUrl;
        }
        catch (Exception ex)
        {
            _logger.LogError(ex, "Failed to process image: {Filename}", filename);
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