<?php
require __DIR__ . '/../vendor/autoload.php';

use Slim\Factory\AppFactory;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

$app = AppFactory::create();

$state = ['hits' => 0];

$app->get('/', function (Request $request, Response $response) {
    $response->getBody()->write(json_encode([
        'app' => 'slim-php',
        'endpoints' => ['/health', '/hit'],
    ]));
    return $response->withHeader('Content-Type', 'application/json');
});

$app->get('/health', function (Request $request, Response $response) use (&$state) {
    $response->getBody()->write(json_encode([
        'status' => 'ok',
        'hits' => $state['hits'],
    ]));
    return $response->withHeader('Content-Type', 'application/json');
});

$app->post('/hit', function (Request $request, Response $response) use (&$state) {
    $state['hits']++;
    $response->getBody()->write(json_encode(['hits' => $state['hits']]));
    return $response->withHeader('Content-Type', 'application/json');
});

$app->run();
