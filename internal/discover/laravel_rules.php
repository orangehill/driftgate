<?php
// DriftGate: prints, for every route, the file and line of its action
// (controller method or closure) and the validation rules of a controller
// method that takes a form request. Read from standard input by `php`, from
// the application's root; written for PHP 7.0 and later and Laravel 5.5 and
// later. It only reads: no request is handled and nothing is stored.

require getcwd() . '/vendor/autoload.php';
$app = require getcwd() . '/bootstrap/app.php';
$app->make('Illuminate\Contracts\Console\Kernel')->bootstrap();

function driftgate_rule_text($rule)
{
    if (is_string($rule)) {
        return $rule;
    }
    if (is_object($rule)) {
        if (method_exists($rule, '__toString')) {
            return (string) $rule;
        }
        $class = get_class($rule);
        $pos = strrpos($class, '\\');
        return 'object:' . ($pos === false ? $class : substr($class, $pos + 1));
    }
    return 'other';
}

// driftgate_source returns "path:line" of a reflected function or method,
// relative to the application's root, or '' for code outside it.
function driftgate_source($reflection)
{
    $file = $reflection->getFileName();
    if (!is_string($file) || $file === '') {
        return '';
    }
    $root = realpath(getcwd());
    $real = realpath($file);
    if ($root === false || $real === false || strpos($real, $root . DIRECTORY_SEPARATOR) !== 0) {
        return '';
    }
    $rel = str_replace(DIRECTORY_SEPARATOR, '/', substr($real, strlen($root) + 1));
    $line = $reflection->getStartLine();
    return $line ? $rel . ':' . $line : $rel;
}

$out = array();
foreach ($app['router']->getRoutes() as $route) {
    $entry = array(
        'methods' => $route->methods(),
        'uri' => $route->uri(),
        'domain' => $route->getDomain(),
        'file' => '',
        'rules' => null,
        'error' => null,
    );
    $action = $route->getActionName();
    if ($action === 'Closure') {
        $definition = $route->getAction();
        if (isset($definition['uses']) && $definition['uses'] instanceof Closure) {
            try {
                $entry['file'] = driftgate_source(new ReflectionFunction($definition['uses']));
            } catch (Throwable $e) {
            }
        }
        $out[] = $entry;
        continue;
    }
    if ($action === '') {
        continue;
    }
    if (strpos($action, '@') === false) {
        $class = $action;
        $method = '__invoke';
    } else {
        list($class, $method) = explode('@', $action, 2);
    }
    if (!class_exists($class)) {
        continue;
    }
    if (!method_exists($class, $method)) {
        try {
            $entry['file'] = driftgate_source(new ReflectionClass($class));
        } catch (Throwable $e) {
        }
        $out[] = $entry;
        continue;
    }
    $rules = null;
    $error = null;
    try {
        $reflection = new ReflectionMethod($class, $method);
        $entry['file'] = driftgate_source($reflection);
        $parameters = $reflection->getParameters();
    } catch (ReflectionException $e) {
        $out[] = $entry;
        continue;
    }
    foreach ($parameters as $parameter) {
        $type = $parameter->getType();
        if ($type === null) {
            continue;
        }
        $name = method_exists($type, 'getName') ? $type->getName() : (string) $type;
        if (!class_exists($name) || !is_subclass_of($name, 'Illuminate\Foundation\Http\FormRequest')) {
            continue;
        }
        try {
            $request = $name::createFrom(Illuminate\Http\Request::create('/' . $route->uri(), 'POST'));
            $request->setContainer($app);
            $rules = method_exists($request, 'rules') ? $app->call(array($request, 'rules')) : array();
        } catch (Throwable $e) {
            $error = get_class($e) . ': ' . $e->getMessage();
        }
        break;
    }
    if ($rules === null && $error === null) {
        $out[] = $entry;
        continue;
    }
    $normalized = array();
    foreach ((array) $rules as $field => $list) {
        if (is_string($list)) {
            $list = explode('|', $list);
        }
        $items = array();
        foreach ((array) $list as $rule) {
            $items[] = driftgate_rule_text($rule);
        }
        $normalized[(string) $field] = $items;
    }
    $entry['rules'] = (object) $normalized;
    $entry['error'] = $error;
    $out[] = $entry;
}
// Text that is not valid UTF-8 (an exception message) must not empty the result.
$flags = JSON_PARTIAL_OUTPUT_ON_ERROR | (defined('JSON_INVALID_UTF8_SUBSTITUTE') ? JSON_INVALID_UTF8_SUBSTITUTE : 0);
echo "\n__DRIFTGATE_RULES__" . json_encode($out, $flags);
